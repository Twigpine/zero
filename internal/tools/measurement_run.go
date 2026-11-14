package tools

import (
	"encoding/json"
	"strings"

	"github.com/Twigpine/zero/internal/measurements"
	"mvdan.cc/sh/v3/syntax"
)

// MeasurementRun preserves the actually executed simple command as provenance.
// Opaque tool names/argument JSON cannot certify who emitted JSON-shaped text.
// Wrappers, compound shell programs, substitutions and redirects are left
// unverified; the ledger then remains silent rather than inventing evidence.
func MeasurementRun(name, arguments, cwd string) measurements.Run {
	unknown := measurements.Run{Command: name, Args: []string{arguments}, Dir: cwd}
	if name != "bash" && name != "exec_command" {
		return unknown
	}
	var fields map[string]any
	if json.Unmarshal([]byte(arguments), &fields) != nil {
		return unknown
	}
	command := ""
	for _, key := range []string{"command", "cmd", "script", "shell"} {
		if text, ok := fields[key].(string); ok && strings.TrimSpace(text) != "" {
			command = text
			break
		}
	}
	if command == "" {
		return unknown
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 {
		return unknown
	}
	statement := file.Stmts[0]
	if statement.Background || statement.Negated || len(statement.Redirs) != 0 {
		return unknown
	}
	call, ok := statement.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) == 0 {
		return unknown
	}
	argv := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, ok := measurementLiteral(word.Parts)
		if !ok {
			return unknown
		}
		argv = append(argv, value)
	}
	return measurements.Run{Command: argv[0], Args: argv[1:], Dir: cwd}
}

func measurementLiteral(parts []syntax.WordPart) (string, bool) {
	var value strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			value.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			text, ok := measurementLiteral(p.Parts)
			if !ok {
				return "", false
			}
			value.WriteString(text)
		default:
			return "", false
		}
	}
	return value.String(), true
}
