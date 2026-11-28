package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// These revisions use the initialize handshake. Newer, handshake-free
// transports require a separate implementation, not another candidate here.
var clientProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var protocolVersionPattern = regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}\b`)
var supportedVersionLabel = regexp.MustCompile(`(?i)\bsupported(?:[\s_-]+(?:protocol[\s_-]+)?versions?)?\s*(?::|=|\bare\b)\s*`)
var protocolVersionPrefix = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\b`)
var protocolVersionConjunction = regexp.MustCompile(`(?i)^(?:and|or)\s+`)

func negotiateProtocol(ctx context.Context, request func(context.Context, string, any, any) error) (string, error) {
	version := clientProtocolVersions[0]
	tried := map[string]bool{}
	for !tried[version] {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		tried[version] = true
		var result struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		err := request(ctx, "initialize", map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "zero", "version": "dev"},
		}, &result)
		if err == nil {
			for _, supported := range clientProtocolVersions {
				if result.ProtocolVersion == supported {
					return supported, nil
				}
			}
			return "", fmt.Errorf("MCP server selected unsupported protocol version %q (client supports %s)", result.ProtocolVersion, strings.Join(clientProtocolVersions, ", "))
		}
		var rejected *rpcError
		if !errors.As(err, &rejected) {
			return "", err
		}
		advertised, versionError := advertisedProtocolVersions(rejected)
		if !versionError {
			return "", err
		}
		next := ""
		for _, candidate := range clientProtocolVersions {
			if advertised[candidate] && !tried[candidate] {
				next = candidate
				break
			}
		}
		if next == "" {
			return "", fmt.Errorf("MCP protocol negotiation exhausted mutually supported versions (client supports %s): %w", strings.Join(clientProtocolVersions, ", "), err)
		}
		version = next
	}
	return "", fmt.Errorf("MCP protocol negotiation repeated version %q", version)
}

// Error data is JSON, so escaped digits and separators must be decoded before
// inspection. Explicit supported lists take precedence over incidental dates
// in messages, requested-version fields, and unsupported lists.
func advertisedProtocolVersions(rejected *rpcError) (map[string]bool, bool) {
	versions := map[string]bool{}
	texts := []string{rejected.Message}
	explicit := false
	var addList func(any)
	addList = func(value any) {
		switch typed := value.(type) {
		case string:
			for _, version := range protocolVersionPattern.FindAllString(typed, -1) {
				versions[version] = true
			}
		case []any:
			for _, item := range typed {
				addList(item)
			}
		}
	}
	var inspect func(any)
	inspect = func(value any) {
		switch typed := value.(type) {
		case string:
			texts = append(texts, typed)
		case []any:
			for _, item := range typed {
				inspect(item)
			}
		case map[string]any:
			for key, item := range typed {
				normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
				switch normalized {
				case "supportedversions", "supportedprotocolversions":
					explicit = true
					addList(item)
				case "unsupportedversions", "unsupportedprotocolversions":
					// These are exclusions, never a source of retry candidates.
				default:
					inspect(item)
				}
			}
		}
	}
	var data any
	if len(rejected.Data) > 0 && json.Unmarshal(rejected.Data, &data) == nil {
		inspect(data)
	}
	if explicit {
		return versions, true
	}
	lower := strings.ToLower(strings.Join(texts, " "))
	if !strings.Contains(lower, "protocol") || !strings.Contains(lower, "version") ||
		(!strings.Contains(lower, "support") && !strings.Contains(lower, "mismatch")) {
		return versions, false
	}
	for _, text := range texts {
		for _, label := range supportedVersionLabel.FindAllStringIndex(text, -1) {
			if strings.HasSuffix(strings.ToLower(strings.TrimSpace(text[:label[0]])), "not") {
				continue
			}
			list := text[label[1]:]
			for {
				list = strings.TrimLeft(list, " \t\r\n,;|/[]()\"'`")
				list = protocolVersionConjunction.ReplaceAllString(list, "")
				version := protocolVersionPrefix.FindString(list)
				if version == "" {
					break
				}
				versions[version] = true
				list = list[len(version):]
			}
		}
	}
	return versions, true
}
