package tools

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/lsp"
)

// lspNavigateTool exposes LSP code navigation (jump-to-definition, find-all-
// references, find-implementations, workspace symbol search) as a read-only,
// model-callable tool. It answers "where is X defined / who calls X / what
// implements this interface" semantically, where grep collides on names or
// misses indirect references. Degrades to a clear "unavailable" message when no
// language server is installed for the file's type — LSP is opportunistic.
type lspNavigateTool struct {
	baseTool
	workspaceRoot string
	scope         PathScope
	manager       *lsp.Manager
}

// NewScopedLSPNavigateTool builds the tool with its own lazily-started LSP
// manager (servers spin up on first use and are reused across calls within a
// session). The model-supplied path is resolved through the same scoped
// confinement the sibling read-only tools use, so a `..` or absolute path
// cannot read or open a file outside the workspace (or an extra granted root).
func NewScopedLSPNavigateTool(workspaceRoot string, scope PathScope) Tool {
	root := normalizeWorkspaceRoot(workspaceRoot)
	return lspNavigateTool{
		baseTool: baseTool{
			name:     "lsp_navigate",
			deferred: true,
			description: "Navigate code semantically via the language server: jump to a symbol's " +
				"definition, find all references, find implementations of an interface/method, or " +
				"search workspace symbols by name. Use this instead of grep when you need precise " +
				"definition/reference/implementation results. Returns file:line:col locations.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"op": {
						Type:        "string",
						Description: "Operation: definition, references, implementation, or workspace_symbol.",
						Enum:        []string{"definition", "references", "implementation", "workspace_symbol"},
					},
					"path":      {Type: "string", Description: "File to anchor the query (required for all ops; for workspace_symbol it selects the language server)."},
					"line":      {Type: "integer", Description: "1-based line of the symbol (definition/references/implementation).", Minimum: intPtr(1)},
					"character": {Type: "integer", Description: "1-based column of the symbol (definition/references/implementation).", Minimum: intPtr(1)},
					"query":     {Type: "string", Description: "Symbol name to search for (workspace_symbol)."},
				},
				Required:             []string{"op", "path"},
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Queries the language server for code navigation; reads files, modifies nothing."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: false, ResourceKeys: fileResourceKeys},
		},
		workspaceRoot: root,
		scope:         scope,
		manager:       lsp.NewManager(root),
	}
}

func (tool lspNavigateTool) Run(ctx context.Context, args map[string]any) Result {
	opRaw, err := stringArg(args, "op", "", true)
	if err != nil {
		return errorResult("Error: Invalid arguments for lsp_navigate: " + err.Error())
	}
	op := lsp.NavOp(strings.ToLower(strings.TrimSpace(opRaw)))

	requestedPath, err := aliasedStringArg(args, []string{"path", "file", "file_path"}, "", true, false)
	if err != nil {
		return errorResult("Error: Invalid arguments for lsp_navigate: " + err.Error())
	}

	// Confine the model-supplied path to the workspace (or an extra granted root)
	// BEFORE reading it or handing it to the language server — a `..` or absolute
	// path must not read/open a file outside the boundary the sibling read-only
	// tools enforce. Errors echo only the relative path, never an absolute one.
	absPath, relPath, scopeErr := resolveScopedReadPath(tool.workspaceRoot, tool.scope, requestedPath)
	if scopeErr != nil {
		return errorResult("Error: lsp_navigate " + scopeErr.Error())
	}

	text, readErr := readWorkspaceFile(absPath, tool.workspaceRoot)
	if readErr != nil {
		return errorResult("Error: lsp_navigate could not read " + relPath + ": " + readErr.Error())
	}

	// Hand the LSP manager the resolved absolute path so it opens only the
	// confined file (not the raw, possibly-escaping, model input).
	req := lsp.NavRequest{Op: op, Path: absPath, Text: text}
	switch op {
	case lsp.NavWorkspaceSymbol:
		query, qErr := stringArg(args, "query", "", true)
		if qErr != nil {
			return errorResult("Error: lsp_navigate workspace_symbol requires a query.")
		}
		req.Query = strings.TrimSpace(query)
	case lsp.NavDefinition, lsp.NavReferences, lsp.NavImplementation:
		line, lErr := intArg(args, "line", 0, 1, 0)
		col, cErr := intArg(args, "character", 1, 1, 0)
		if lErr != nil || cErr != nil || line == 0 {
			return errorResult("Error: lsp_navigate " + string(op) + " requires 1-based line and character.")
		}
		req.Line = line
		req.Character = col
		req.IncludeDeclaration = true
	default:
		return errorResult("Error: lsp_navigate unknown op " + string(op) + " (use definition, references, implementation, or workspace_symbol).")
	}

	locations, symbols, ok, navErr := tool.manager.Navigate(ctx, req)
	if navErr != nil {
		return errorResult("Error: lsp_navigate failed: " + navErr.Error())
	}
	if !ok {
		return okResult(fmt.Sprintf("No language server available for %s — lsp_navigate is unavailable for this file type. Fall back to grep/read_file.", filepath.Base(relPath)))
	}

	return okResult(tool.formatResult(op, locations, symbols))
}

// readWorkspaceFile binds the confined path and credential check to the handle
// supplying the text, rather than reopening an authorized pathname.
func readWorkspaceFile(absPath, workspaceRoot string) (string, error) {
	file, _, err := ProtectedReadOpen(absPath, workspaceRoot)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (tool lspNavigateTool) formatResult(op lsp.NavOp, locations []lsp.Location, symbols []lsp.SymbolResult) string {
	if op == lsp.NavWorkspaceSymbol {
		if len(symbols) == 0 {
			return "No matching symbols."
		}
		lines := make([]string, 0, len(symbols)+1)
		lines = append(lines, fmt.Sprintf("%d symbol(s):", len(symbols)))
		for _, s := range symbols {
			lines = append(lines, fmt.Sprintf("  %s %s — %s", s.Kind, s.Name, tool.formatLocation(s.Location)))
		}
		return strings.Join(lines, "\n")
	}
	if len(locations) == 0 {
		return "No " + string(op) + " results."
	}
	lines := make([]string, 0, len(locations)+1)
	lines = append(lines, fmt.Sprintf("%d %s result(s):", len(locations), op))
	for _, l := range locations {
		lines = append(lines, "  "+tool.formatLocation(l))
	}
	return strings.Join(lines, "\n")
}

// formatLocation renders a location as a workspace-relative file:line:col
// (1-based for the user, converting from LSP's 0-based positions).
func (tool lspNavigateTool) formatLocation(l lsp.Location) string {
	path := lsp.URIToPath(l.URI)
	if rel, err := filepath.Rel(tool.workspaceRoot, path); err == nil && !strings.HasPrefix(rel, "..") {
		path = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("%s:%d:%d", path, l.Range.Start.Line+1, l.Range.Start.Character+1)
}
