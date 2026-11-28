package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A record of the paths a sandbox principal was last granted ACEs on.
//
// applyWindowsPrincipalACLs revokes this trustee before it re-applies, but the
// only paths it could name were the ones in the plan it was about to apply. A
// root that LEFT the policy is absent from that plan, so its ACE survived the
// re-setup marker validation forces and the principal kept write access the
// current policy no longer grants — the sandbox widened as a result of being
// tightened. Teardown repeated the same current-plan-only calculation, so it did
// not clean the leftover either.
//
// Nothing on Windows can answer "which paths hold an ACE for this SID" without
// walking every volume, so the grants have to be written down as they are made.
//
// Keyed by principal, beside the secret and for the same reason: one sandbox
// home serves every workspace on the machine, so a single shared file would let
// one workspace's setup overwrite another's record — reproducing exactly the
// stale-ACE bug this exists to close, one level up.

const windowsPrincipalACLLedgerSchemaVersion = 1

const windowsPrincipalACLLedgerDirName = "windows-principal-acl"

type windowsPrincipalACLLedger struct {
	SchemaVersion int      `json:"schemaVersion"`
	Paths         []string `json:"paths"`
}

func windowsPrincipalACLLedgerPath(sandboxHome string, username string) (string, error) {
	if strings.TrimSpace(sandboxHome) == "" {
		return "", errors.New("windows principal ACL ledger: empty sandbox home")
	}
	if strings.TrimSpace(username) == "" {
		return "", errors.New("windows principal ACL ledger: empty principal name")
	}
	// The same guard the secret path applies, against a caller passing something
	// windowsSandboxUserName did not produce.
	if strings.ContainsAny(username, `\/:`) || strings.Contains(username, "..") {
		return "", fmt.Errorf("windows principal ACL ledger: unsafe principal name %q", username)
	}
	return filepath.Join(sandboxHome, windowsPrincipalACLLedgerDirName, username+".json"), nil
}

// readWindowsPrincipalACLLedger returns the paths an earlier setup recorded for
// this principal.
//
// recorded is false for every reason the record cannot be trusted — absent,
// unreadable, malformed, or written to a schema this build does not know — and
// not merely for "absent". Collapsing them is deliberate: there is exactly one
// safe response to any of them, and it is the same one. The prior grant set is
// unknown, and a principal whose grants are unknown cannot be reused. Returning
// an error instead would invite a caller to report it and carry on with an empty
// prior set, which is the fail-open this record exists to close — the one case
// where the previous paths are not empty but unenumerable.
func readWindowsPrincipalACLLedger(sandboxHome string, username string) ([]string, bool) {
	path, err := windowsPrincipalACLLedgerPath(sandboxHome, username)
	if err != nil {
		return nil, false
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var ledger windowsPrincipalACLLedger
	if err := json.Unmarshal(contents, &ledger); err != nil {
		return nil, false
	}
	if ledger.SchemaVersion != windowsPrincipalACLLedgerSchemaVersion {
		return nil, false
	}
	return trimNonEmptyStrings(ledger.Paths), true
}

// writeWindowsPrincipalACLLedger records paths for this principal, replacing any
// previous record atomically so an interrupted write cannot leave a truncated
// file — which the reader would then treat as "no principal was ever granted
// anything", the very state it must never guess.
func writeWindowsPrincipalACLLedger(sandboxHome string, username string, paths []string) error {
	path, err := windowsPrincipalACLLedgerPath(sandboxHome, username)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create windows principal ACL ledger dir: %w", err)
	}
	contents, err := json.MarshalIndent(windowsPrincipalACLLedger{
		SchemaVersion: windowsPrincipalACLLedgerSchemaVersion,
		Paths:         trimNonEmptyStrings(paths),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal windows principal ACL ledger: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".windows-principal-acl-*.tmp")
	if err != nil {
		return fmt.Errorf("create windows principal ACL ledger temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write windows principal ACL ledger temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close windows principal ACL ledger temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace windows principal ACL ledger: %w", err)
	}
	return nil
}

// removeWindowsPrincipalACLLedger drops the record. An absent one is not an
// error: this runs on the teardown path, where being gone is the goal.
func removeWindowsPrincipalACLLedger(sandboxHome string, username string) error {
	path, err := windowsPrincipalACLLedgerPath(sandboxHome, username)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove windows principal ACL ledger: %w", err)
	}
	return nil
}

// unionWindowsPrincipalACLPaths merges path sets for revocation, keeping the
// first spelling of each path.
//
// Deduplication uses the same case-insensitive key the ACL plans use, so a root
// recorded as C:\Ws by one setup and re-granted as c:\ws by the next is one path
// to revoke rather than two.
func unionWindowsPrincipalACLPaths(sets ...[]string) []string {
	seen := make(map[string]struct{})
	union := make([]string, 0)
	for _, set := range sets {
		for _, path := range set {
			key := windowsCapabilityPathKey(path)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			union = append(union, path)
		}
	}
	return union
}
