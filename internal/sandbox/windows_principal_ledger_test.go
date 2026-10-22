package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The point of the record is that a LATER setup can name a root the CURRENT
// policy no longer mentions, so the round trip has to survive the process that
// wrote it having no memory of the paths.
func TestPrincipalACLLedgerRoundTripsRecordedPaths(t *testing.T) {
	home := t.TempDir()
	paths := []string{`C:\ws\alpha`, `C:\ws\beta`, `C:\cache\runtime`}
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx01", paths); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, recorded := readWindowsPrincipalACLLedger(home, "zerosbx01")
	if !recorded {
		t.Fatal("a record this process just wrote read back as untrusted")
	}
	if strings.Join(got, "|") != strings.Join(paths, "|") {
		t.Errorf("read back %v, want %v", got, paths)
	}
}

// One sandbox home serves every workspace on the machine. If the record were a
// single shared file, workspace B's setup would overwrite workspace A's, and A's
// dropped roots would then be unnameable at the next re-setup — the same stale
// ACE this record exists to revoke, produced by the record itself.
func TestPrincipalACLLedgerIsPerPrincipal(t *testing.T) {
	home := t.TempDir()
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx01", []string{`C:\ws\alpha`}); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx02", []string{`C:\ws\beta`}); err != nil {
		t.Fatalf("write beta: %v", err)
	}
	alpha, ok := readWindowsPrincipalACLLedger(home, "zerosbx01")
	if !ok || len(alpha) != 1 || alpha[0] != `C:\ws\alpha` {
		t.Errorf("first principal's record = %v (ok=%v); a second workspace's setup overwrote it", alpha, ok)
	}
}

// Every untrustworthy record has to read as untrustworthy, not as "nothing was
// ever granted". The caller retires the principal on false; treating a corrupt
// file as an empty prior set is the fail-open.
func TestPrincipalACLLedgerRefusesRecordsItCannotTrust(t *testing.T) {
	for name, contents := range map[string]string{
		"truncated mid-write":  `{"schemaVersion": 1, "pat`,
		"not json at all":      "\x00\x01garbage",
		"a schema from later":  `{"schemaVersion": 99, "paths": ["C:\\ws"]}`,
		"a schema from before": `{"paths": ["C:\\ws"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path, err := windowsPrincipalACLLedgerPath(home, "zerosbx01")
			if err != nil {
				t.Fatalf("path: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if paths, recorded := readWindowsPrincipalACLLedger(home, "zerosbx01"); recorded {
				t.Errorf("read a record it cannot interpret as trustworthy (%v); the caller would then revoke nothing", paths)
			}
		})
	}
	// And an absent one, which is the ordinary first-setup case.
	if _, recorded := readWindowsPrincipalACLLedger(t.TempDir(), "zerosbx01"); recorded {
		t.Error("a missing record read as trusted")
	}
}

// A partial write must not be readable at all, which is why the file is renamed
// into place rather than written in situ: a reader that saw half a record would
// treat the missing half as never granted.
func TestPrincipalACLLedgerWriteIsAtomic(t *testing.T) {
	home := t.TempDir()
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx01", []string{`C:\ws\alpha`, `C:\ws\beta`}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx01", []string{`C:\ws\alpha`}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	path, err := windowsPrincipalACLLedgerPath(home, "zerosbx01")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var ledger windowsPrincipalACLLedger
	if err := json.Unmarshal(contents, &ledger); err != nil {
		t.Fatalf("the replaced record did not parse: %v", err)
	}
	if len(ledger.Paths) != 1 {
		t.Errorf("record = %v, want the second write to have replaced the first outright", ledger.Paths)
	}
	// No temp files left behind to be mistaken for a record later.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ledger directory holds %d entries, want just the record", len(entries))
	}
}

// The name comes from windowsSandboxUserName, but the path builder is the last
// thing between a caller and the filesystem, so it refuses anything that could
// escape the directory.
func TestPrincipalACLLedgerPathRefusesUnsafeNames(t *testing.T) {
	for _, username := range []string{"", "  ", `..\..\evil`, "a/b", `a\b`, "c:evil", "..", "zerosbx..01"} {
		if _, err := windowsPrincipalACLLedgerPath(`C:\home`, username); err == nil {
			t.Errorf("accepted principal name %q", username)
		}
	}
	if _, err := windowsPrincipalACLLedgerPath("", "zerosbx01"); err == nil {
		t.Error("accepted an empty sandbox home")
	}
	if _, err := windowsPrincipalACLLedgerPath(`C:\home`, "zerosbx01"); err != nil {
		t.Errorf("rejected a name windowsSandboxUserName would produce: %v", err)
	}
}

// Removal is idempotent because teardown only cares that the record is gone,
// and a setup that failed before writing one must not make teardown fail too.
func TestPrincipalACLLedgerRemovalToleratesAnAbsentRecord(t *testing.T) {
	home := t.TempDir()
	if err := removeWindowsPrincipalACLLedger(home, "zerosbx01"); err != nil {
		t.Fatalf("remove an absent record: %v", err)
	}
	if err := writeWindowsPrincipalACLLedger(home, "zerosbx01", []string{`C:\ws`}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := removeWindowsPrincipalACLLedger(home, "zerosbx01"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, recorded := readWindowsPrincipalACLLedger(home, "zerosbx01"); recorded {
		t.Error("the record survived removal")
	}
}

// The union is what setup revokes over. A path in both sets must be revoked
// once, and a root respelled between setups — Windows opens a path whatever its
// casing — is one path, not two.
func TestUnionPrincipalACLPathsDedupesTheWayTheACLPlansDo(t *testing.T) {
	union := unionWindowsPrincipalACLPaths(
		[]string{`C:\Ws\Alpha`, `C:\ws\beta`, "  "},
		[]string{`c:\ws\alpha`, `C:\ws\gamma`, `C:/ws/beta`},
	)
	if len(union) != 3 {
		t.Fatalf("union = %v, want three distinct paths", union)
	}
	// The first spelling wins: revocation needs a real path, and the recorded one
	// is the spelling that was actually granted.
	if union[0] != `C:\Ws\Alpha` {
		t.Errorf("union[0] = %q, want the recorded spelling kept", union[0])
	}
	// The recorded set comes first so a dropped root cannot be crowded out.
	if union[1] != `C:\ws\beta` || union[2] != `C:\ws\gamma` {
		t.Errorf("union = %v, want the recorded paths before the newly granted ones", union)
	}
	if got := unionWindowsPrincipalACLPaths(nil, nil); len(got) != 0 {
		t.Errorf("union of nothing = %v, want empty", got)
	}
}
