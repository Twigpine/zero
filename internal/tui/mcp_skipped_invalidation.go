package tui

import (
	"reflect"
	"strings"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// adoptMCPConfig replaces the in-session MCP config and drops the startup
// failures that are no longer about anything.
//
// A SKIPPED ENTRY IS AN OBSERVATION ABOUT A PARTICULAR SERVER, NOT ABOUT A NAME.
// m.mcpSkipped is written once, at construction, from the startup registration.
// Every successful /mcp command and the add wizard replaced m.mcpConfig and
// refreshed the view while leaving that snapshot untouched, and failures are
// matched by name alone. So removing a failed "docs" endpoint and adding a
// different URL under the same name made the replacement inherit the dead
// endpoint's error and its failed state, and that false result was written into
// the command transcript as well as the panel.
//
// Nothing in the TUI re-registers MCP tools, so the replacement is not running
// either. That does not make "failed" honest: it is reported as having failed
// for a reason belonging to a server that no longer exists, which sends the
// operator to debug the wrong thing. Dropping the observation leaves the row
// reporting configuration, which is what it can actually know.
func (m model) adoptMCPConfig(next config.MCPConfig) model {
	m.mcpSkipped = retainedMCPSkipped(m.mcpSkipped, m.mcpConfig, next)
	m.mcpConfig = next
	return m
}

// retainedMCPSkipped keeps only the observations whose subject survived the
// change unaltered.
func retainedMCPSkipped(skipped []mcp.SkippedServer, previous config.MCPConfig, next config.MCPConfig) []mcp.SkippedServer {
	if len(skipped) == 0 {
		return skipped
	}
	before := canonicalMCPServers(previous)
	after := canonicalMCPServers(next)
	kept := make([]mcp.SkippedServer, 0, len(skipped))
	for _, entry := range skipped {
		name := strings.TrimSpace(entry.Name)
		current, present := after[name]
		if !present {
			// Removed. The observation has no subject any more.
			continue
		}
		original, existed := before[name]
		if !existed {
			// The name was not configured when this snapshot was taken, so whatever
			// is there now is not what failed.
			continue
		}
		// Compared on the WHOLE config rather than on a rendered target, because
		// the rendered form is redacted: two different credentials print
		// identically, and a rotated token is a different server for this purpose.
		if !reflect.DeepEqual(original, current) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// canonicalMCPServers keys the configured servers the way registration does, so
// a padded config key and the trimmed name recorded in a SkippedServer refer to
// the same entry.
//
// DISABLED ENTRIES ARE EXCLUDED, because registration excludes them
// (NormalizeConfig skips raw.Disabled) and an observation is about a server that
// actually ran. ValidateUniqueNames deliberately accepts an enabled "docs"
// alongside a disabled " docs" for that reason. Copying both into a map keyed by
// the trimmed name made them collide, and Go randomises map iteration, so the
// before and after snapshots each picked a winner independently. An unrelated
// /mcp operation that left the enabled entry untouched could then compare it
// against the disabled alias, find them different, and discard the failure: a
// server that is still unavailable reported as fine, on some runs and not others.
// Measured at 20% of runs on an unchanged config before this.
//
// A canonical name claimed by two ENABLED entries is ambiguous rather than
// arbitrary. ValidateUniqueNames rejects that config so it should not arrive
// here, but if it does there is no way to say which entry an observation was
// about, so the name is dropped and the observation ages out with it.
func canonicalMCPServers(cfg config.MCPConfig) map[string]config.MCPServerConfig {
	servers := make(map[string]config.MCPServerConfig, len(cfg.Servers))
	ambiguous := make(map[string]struct{})
	for name, server := range cfg.Servers {
		if server.Disabled {
			continue
		}
		canonical := strings.TrimSpace(name)
		if _, clash := servers[canonical]; clash {
			ambiguous[canonical] = struct{}{}
			continue
		}
		servers[canonical] = server
	}
	for canonical := range ambiguous {
		delete(servers, canonical)
	}
	return servers
}
