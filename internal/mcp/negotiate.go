package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// These revisions use the initialize handshake. Newer, handshake-free
// transports require a separate implementation, not another candidate here.
var clientProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

var protocolVersionPattern = regexp.MustCompile(`\b[0-9]{4}-[0-9]{2}-[0-9]{2}\b`)

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
		detail := rejected.Message + " " + string(rejected.Data)
		lower := strings.ToLower(detail)
		versionError := (strings.Contains(lower, "protocol") && strings.Contains(lower, "version")) || strings.Contains(lower, "supportedversions")
		if !versionError || (!strings.Contains(lower, "support") && !strings.Contains(lower, "mismatch")) {
			return "", err
		}
		advertised := map[string]bool{}
		for _, candidate := range protocolVersionPattern.FindAllString(detail, -1) {
			advertised[candidate] = true
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
