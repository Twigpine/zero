package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

func TestGrantStorePersistsListsRevokesAndClears(t *testing.T) {
	store, err := NewGrantStore(StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json"),
		Now:      fixedSandboxTime("2026-06-05T14:30:00Z"),
	})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}

	if _, err := store.Grant(GrantInput{ToolName: "bash", Decision: GrantDeny, Reason: "network blocked"}); err != nil {
		t.Fatalf("Grant deny returned error: %v", err)
	}
	allowed, err := store.Grant(GrantInput{ToolName: "write_file", Decision: GrantAllow, Reason: "workspace edits"})
	if err != nil {
		t.Fatalf("Grant allow returned error: %v", err)
	}
	if allowed.ApprovedAt != "2026-06-05T14:30:00Z" {
		t.Fatalf("approvedAt = %q, want fixed timestamp", allowed.ApprovedAt)
	}

	reopened, err := NewGrantStore(StoreOptions{FilePath: store.FilePath()})
	if err != nil {
		t.Fatalf("reopen grant store: %v", err)
	}
	grants, err := reopened.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(grants) != 2 || grants[0].ToolName != "bash" || grants[1].ToolName != "write_file" {
		t.Fatalf("unexpected sorted grants: %#v", grants)
	}

	match, err := reopened.Lookup("write_file", "")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if !match.Matched || match.Grant.Decision != GrantAllow {
		t.Fatalf("lookup allow = %#v, want matched allow", match)
	}

	revoked, err := reopened.Revoke("bash")
	if err != nil {
		t.Fatalf("Revoke returned error: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}
	cleared, err := reopened.Clear()
	if err != nil {
		t.Fatalf("Clear returned error: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}
	grants, err = reopened.List()
	if err != nil {
		t.Fatalf("List after clear returned error: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected no grants after clear, got %#v", grants)
	}
}

func TestGrantStoreSerializesWritesAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	storeA, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore A returned error: %v", err)
	}
	storeB, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore B returned error: %v", err)
	}

	unlock, err := storeA.lockStateFile()
	if err != nil {
		t.Fatalf("lockStateFile returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := storeB.Grant(GrantInput{ToolName: "write_file", Decision: GrantAllow})
		done <- err
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("Grant completed while another store held the file lock: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: storeB is waiting for storeA to release the lock.
	}

	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Grant returned error after file lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Grant did not complete after file lock release")
	}
}

// TestGrantStoreSerializesMigrationNoticeAcrossStores covers the other
// read-modify-write on the state file: clearing the pending flag. Two frontends
// starting at once must not both read it as pending and clobber each other.
func TestGrantStoreSerializesMigrationNoticeAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	if err := writeText(path, `{"schemaVersion":1,"grants":{"write_file":{"toolName":"write_file","decision":"allow","approvedAt":"2026-06-05T14:30:00Z"}}}`); err != nil {
		t.Fatalf("write v1 grants: %v", err)
	}
	storeA, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore A returned error: %v", err)
	}
	storeB, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore B returned error: %v", err)
	}
	// Migrate first, so the notice is pending and the lock contention below is
	// about consuming it rather than about the migration write.
	if _, err := storeA.List(); err != nil {
		t.Fatalf("List returned error: %v", err)
	}

	unlock, err := storeA.lockStateFile()
	if err != nil {
		t.Fatalf("lockStateFile returned error: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		notice, err := storeB.ConsumeMigrationNotice()
		if err == nil && notice == "" {
			err = errors.New("expected the pending migration notice")
		}
		done <- err
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("ConsumeMigrationNotice completed while another store held the file lock: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: storeB is waiting for storeA to release the lock.
	}

	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ConsumeMigrationNotice after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ConsumeMigrationNotice did not complete after file lock release")
	}
	// The flag write landed, so the notice is not reported twice.
	if again, err := storeA.ConsumeMigrationNotice(); err != nil || again != "" {
		t.Fatalf("second ConsumeMigrationNotice = %q err=%v, want empty", again, err)
	}
}

// TestConsumeMigrationNoticeWritesNothingWithoutAPendingNotice is the regression
// test for jatmn's #755 finding: ConsumeMigrationNotice took the interprocess
// lock unconditionally, and acquiring that lock MkdirAlls the grants directory
// and creates <grants>.lockfile. Every startup and every `zero exec` calls this,
// so it turned "read the grants state" into "require a writable grants
// directory" — a user with no grants file whose grants path sat on a read-only
// mount failed outright with "failed to migrate sandbox grants".
//
// Asserting that nothing is created is the portable form of that requirement: it
// holds identically on every OS and as any user, unlike a read-only-directory
// test (see the sibling test, which cannot run everywhere).
func TestConsumeMigrationNoticeWritesNothingWithoutAPendingNotice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	path := filepath.Join(dir, "sandbox-grants.json")
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}

	notice, err := store.ConsumeMigrationNotice()
	if err != nil {
		t.Fatalf("ConsumeMigrationNotice with no grants file: %v", err)
	}
	if notice != "" {
		t.Fatalf("notice = %q, want empty when nothing was migrated", notice)
	}
	// Nothing at all may be created: not the lock file, not the grants file, not
	// even the directory that would hold them.
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the grants directory was created (contents: %v); reading a state with no pending notice must not require a writable directory", names)
	}
}

// TestConsumeMigrationNoticeToleratesAReadOnlyGrantsDirectory exercises the
// actual failure from the same finding, rather than only its portable proxy
// above: with the grants directory read-only and no grants file, the pre-fix
// code failed at MkdirAll/OpenFile for the lock file.
//
// It cannot run everywhere — Windows ignores the mode bits used here, and root
// bypasses directory permissions entirely — so it skips instead of pretending to
// cover those platforms.
func TestConsumeMigrationNoticeToleratesAReadOnlyGrantsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so a read-only directory cannot be simulated")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-grants.json")
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the grants directory read-only: %v", err)
	}
	// Restore write permission so t.TempDir's cleanup can remove the directory.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	// Confirm the directory really is read-only here; otherwise this test would
	// pass without exercising anything.
	if probe, err := os.OpenFile(filepath.Join(dir, ".probe"), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		_ = probe.Close()
		t.Skip("this filesystem still allows writes to a 0500 directory")
	}

	notice, err := store.ConsumeMigrationNotice()
	if err != nil {
		t.Fatalf("ConsumeMigrationNotice on a read-only grants directory: %v", err)
	}
	if notice != "" {
		t.Fatalf("notice = %q, want empty when nothing was migrated", notice)
	}
}

// TestGrantStoreMigrationTakesTheFileLock covers the migrations that write
// through readState: a legacy file must not be rewritten (nor its backup written)
// while another process holds the lock, or two processes could both migrate and
// one would clobber the other's result.
func TestGrantStoreMigrationTakesTheFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	original := `{"schemaVersion":1,"grants":{"write_file":{"toolName":"write_file","decision":"allow","approvedAt":"2026-06-05T14:30:00Z"}}}`
	if err := writeText(path, original); err != nil {
		t.Fatalf("write v1 grants: %v", err)
	}
	storeA, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore A returned error: %v", err)
	}
	storeB, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore B returned error: %v", err)
	}

	unlock, err := storeA.lockStateFile()
	if err != nil {
		t.Fatalf("lockStateFile returned error: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := storeB.List()
		done <- err
	}()

	select {
	case err := <-done:
		unlock()
		t.Fatalf("the migration ran while another store held the file lock: %v", err)
	case <-time.After(100 * time.Millisecond):
		if raw, err := os.ReadFile(path); err != nil {
			unlock()
			t.Fatalf("ReadFile grants: %v", err)
		} else if string(raw) != original {
			unlock()
			t.Fatalf("the grant file was rewritten while the lock was held:\n%s", raw)
		}
	}

	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("List after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the migration did not complete after file lock release")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile migrated grants: %v", err)
	}
	if !strings.Contains(string(raw), `"schemaVersion": 3`) {
		t.Fatalf("grant file was not migrated after the lock was released:\n%s", raw)
	}
}

func TestGrantStoreMigratesExactV1GrantAndReportsOnce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sandbox-grants.json")
	original := `{"schemaVersion":1,"grants":{"write_file":{"toolName":"write_file","decision":"allow","approvedAt":"2026-06-05T14:30:00Z","reason":"legacy"}}}`
	if err := writeText(path, original); err != nil {
		t.Fatalf("write v1 grants: %v", err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path, Now: fixedSandboxTime("2026-06-05T15:00:00Z")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}

	grants, err := store.List()
	if err != nil {
		t.Fatalf("List v1 grants returned error: %v", err)
	}
	if len(grants) != 1 || grants[0].ToolName != "write_file" || grants[0].Decision != GrantAllow || grants[0].Reason != "legacy" {
		t.Fatalf("unexpected v1 grants: %#v", grants)
	}
	notice, err := store.ConsumeMigrationNotice()
	if err != nil || !strings.Contains(notice, "migrated 1, invalidated 0") {
		t.Fatalf("migration notice = %q err=%v", notice, err)
	}
	if again, err := store.ConsumeMigrationNotice(); err != nil || again != "" {
		t.Fatalf("second migration notice = %q err=%v, want empty", again, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten grant file: %v", err)
	}
	policyLine := fmt.Sprintf(`"policyVersion": %d`, execution.PolicyVersion)
	if !strings.Contains(string(raw), `"schemaVersion": 3`) || !strings.Contains(string(raw), policyLine) || !strings.Contains(string(raw), `"write_file": [`) {
		t.Fatalf("grant file was not rewritten as a versioned grant store:\n%s", raw)
	}
	backup, err := os.ReadFile(path + ".v1.backup")
	if err != nil || string(backup) != original {
		t.Fatalf("migration backup = %q err=%v, want original", backup, err)
	}
}

func TestGrantStoreInvalidatesLegacyShellAllowButKeepsSafePrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	original := `{"schemaVersion":2,"grants":{"exec_command":[{"toolName":"exec_command","decision":"allow","approvedAt":"2026-06-05T14:30:00Z"}]},"commandPrefixes":{"exec_command":[{"toolName":"exec_command","prefix":["git","status"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`
	if err := writeText(path, original); err != nil {
		t.Fatal(err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := store.List()
	if err != nil || len(grants) != 0 {
		t.Fatalf("legacy shell allows = %#v err=%v, want invalidated", grants, err)
	}
	prefixes, err := store.ListCommandPrefixes()
	if err != nil || len(prefixes) != 1 || !sameStringSlice(prefixes[0].Prefix, []string{"git", "status"}) {
		t.Fatalf("migrated prefixes = %#v err=%v", prefixes, err)
	}
	notice, err := store.ConsumeMigrationNotice()
	if err != nil || !strings.Contains(notice, "migrated 1, invalidated 1") {
		t.Fatalf("notice = %q err=%v", notice, err)
	}
	backup, err := os.ReadFile(path + ".v2.backup")
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q err=%v", backup, err)
	}
}

func TestGrantStorePolicyChangePreservesDeniesAndInvalidatesApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	original := `{"schemaVersion":3,"policyVersion":0,"grants":{"write_file":[{"toolName":"write_file","decision":"allow","approvedAt":"2026-06-05T14:30:00Z"}],"bash":[{"toolName":"bash","decision":"deny","approvedAt":"2026-06-05T14:30:00Z"}]},"commandPrefixes":{"exec_command":[{"toolName":"exec_command","prefix":["git","status"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`
	if err := writeText(path, original); err != nil {
		t.Fatal(err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := store.List()
	if err != nil || len(grants) != 1 || grants[0].Decision != GrantDeny {
		t.Fatalf("migrated policy grants = %#v err=%v, want deny only", grants, err)
	}
	if prefixes, err := store.ListCommandPrefixes(); err != nil || len(prefixes) != 0 {
		t.Fatalf("changed-policy prefixes = %#v err=%v, want invalidated", prefixes, err)
	}
	if notice, err := store.ConsumeMigrationNotice(); err != nil || !strings.Contains(notice, "migrated 1, invalidated 2") {
		t.Fatalf("notice = %q err=%v", notice, err)
	}
	backup, err := os.ReadFile(path + ".policy-v0.backup")
	if err != nil || string(backup) != original {
		t.Fatalf("policy backup = %q err=%v", backup, err)
	}
}

// A prefix grant recorded before launcher-name normalization existed (a
// versioned or .exe-suffixed interpreter) no longer validates. It must reach
// the user through the policy migration — backup, denies preserved, one notice
// — and never make the whole grants file unreadable, which would silently drop
// their persisted deny grants too.
func TestGrantStoreMigratesPreNormalizationPrefixInsteadOfRejectingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	original := `{"schemaVersion":3,"policyVersion":1,"grants":{"bash":[{"toolName":"bash","decision":"deny","approvedAt":"2026-06-05T14:30:00Z"}]},"commandPrefixes":{"bash":[{"toolName":"bash","prefix":["python3.11","-c"],"approvedAt":"2026-06-05T14:30:00Z"},{"toolName":"bash","prefix":["cargo","build"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`
	if err := writeText(path, original); err != nil {
		t.Fatal(err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := store.List()
	if err != nil || len(grants) != 1 || grants[0].Decision != GrantDeny {
		t.Fatalf("grants after migration = %#v err=%v, want the deny preserved", grants, err)
	}
	prefixes, err := store.ListCommandPrefixes()
	if err != nil || len(prefixes) != 0 {
		t.Fatalf("prefixes after migration = %#v err=%v, want all re-approved", prefixes, err)
	}
	if notice, err := store.ConsumeMigrationNotice(); err != nil || !strings.Contains(notice, "invalidated 2") {
		t.Fatalf("notice = %q err=%v", notice, err)
	}
	if backup, err := os.ReadFile(path + ".policy-v1.backup"); err != nil || string(backup) != original {
		t.Fatalf("backup = %q err=%v", backup, err)
	}
}

// The legacy-schema path is a separate migration from the policy bump above and
// prunes per grant rather than wholesale, so a pre-normalization prefix must be
// dropped there while the valid prefix beside it survives.
func TestGrantStoreLegacyMigrationDropsOnlyThePreNormalizationPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-grants.json")
	original := `{"schemaVersion":2,"grants":{},"commandPrefixes":{"bash":[{"toolName":"bash","prefix":["python3.11","-c"],"approvedAt":"2026-06-05T14:30:00Z"},{"toolName":"bash","prefix":["git","status"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`
	if err := writeText(path, original); err != nil {
		t.Fatal(err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	prefixes, err := store.ListCommandPrefixes()
	if err != nil {
		t.Fatalf("ListCommandPrefixes after legacy migration returned error: %v", err)
	}
	if len(prefixes) != 1 || !sameStringSlice(prefixes[0].Prefix, []string{"git", "status"}) {
		t.Fatalf("prefixes = %#v, want only [git status]", prefixes)
	}
	if notice, err := store.ConsumeMigrationNotice(); err != nil || !strings.Contains(notice, "migrated 1, invalidated 1") {
		t.Fatalf("notice = %q err=%v", notice, err)
	}
}

func TestGrantStorePersistsCommandPrefixes(t *testing.T) {
	store, err := NewGrantStore(StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json"),
		Now:      fixedSandboxTime("2026-06-05T14:30:00Z"),
	})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	grant, err := store.GrantCommandPrefix(CommandPrefixInput{
		ToolName: "bash",
		Prefix:   []string{"git", "status"},
		Reason:   "status checks",
	})
	if err != nil {
		t.Fatalf("GrantCommandPrefix returned error: %v", err)
	}
	if grant.ApprovedAt != "2026-06-05T14:30:00Z" {
		t.Fatalf("approvedAt = %q, want fixed timestamp", grant.ApprovedAt)
	}

	// Re-granting the same prefix updates instead of duplicating.
	if _, err := store.GrantCommandPrefix(CommandPrefixInput{ToolName: "bash", Prefix: []string{"git", "status"}, Reason: "updated"}); err != nil {
		t.Fatalf("GrantCommandPrefix update returned error: %v", err)
	}
	reopened, err := NewGrantStore(StoreOptions{FilePath: store.FilePath()})
	if err != nil {
		t.Fatalf("reopen grant store: %v", err)
	}
	prefixes, err := reopened.ListCommandPrefixes()
	if err != nil {
		t.Fatalf("ListCommandPrefixes returned error: %v", err)
	}
	if len(prefixes) != 1 || prefixes[0].ToolName != "bash" || !sameStringSlice(prefixes[0].Prefix, []string{"git", "status"}) || prefixes[0].Reason != "updated" {
		t.Fatalf("unexpected command prefixes: %#v", prefixes)
	}
	match, matched, err := reopened.LookupCommandPrefix("bash", []string{"git", "status", "--short"})
	if err != nil {
		t.Fatalf("LookupCommandPrefix returned error: %v", err)
	}
	if !matched || !sameStringSlice(match.Prefix, []string{"git", "status"}) {
		t.Fatalf("lookup = (%#v,%t), want git status match", match, matched)
	}
	if _, matched, err := reopened.LookupCommandPrefix("bash", []string{"git", "diff"}); err != nil || matched {
		t.Fatalf("git diff lookup = matched %t err %v, want no match", matched, err)
	}
	text := FormatGrantListWithCommandPrefixes(nil, prefixes)
	for _, want := range []string{"Sandbox Grants:", "bash", "`git status`", "command-prefix", "updated"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted grants = %q, missing %q", text, want)
		}
	}

	revoked, err := reopened.Revoke("bash")
	if err != nil {
		t.Fatalf("Revoke returned error: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d, want 1", revoked)
	}
	if prefixes, err := reopened.ListCommandPrefixes(); err != nil || len(prefixes) != 0 {
		t.Fatalf("prefixes after revoke = %#v err %v, want none", prefixes, err)
	}
}

func TestGrantStoreRejectsUnsafeCommandPrefixes(t *testing.T) {
	store, err := NewGrantStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	for _, prefix := range [][]string{
		{"find"},
		{"xargs"},
		{"python", "script.py"},
		{"./script.sh"},
		{"git"},
		{"python3.11", "-c"},
		{"python.exe", "-c"},
		{"node.exe", "-e"},
		{"git.exe"},
		{"cmd", "/c"},
	} {
		if _, err := store.GrantCommandPrefix(CommandPrefixInput{ToolName: "bash", Prefix: prefix}); err == nil {
			t.Fatalf("GrantCommandPrefix(%#v) succeeded, want validation error", prefix)
		}
	}
}

func TestGrantStoreRejectsUnsafeInputsAndMalformedFiles(t *testing.T) {
	root := t.TempDir()
	store, err := NewGrantStore(StoreOptions{FilePath: filepath.Join(root, "sandbox-grants.json")})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	for _, input := range []GrantInput{
		{ToolName: "", Decision: GrantAllow},
		{ToolName: "../escape", Decision: GrantAllow},
		{ToolName: "write_file", Decision: GrantDecision("maybe")},
	} {
		if _, err := store.Grant(input); err == nil {
			t.Fatalf("Grant(%#v) succeeded, want validation error", input)
		}
	}

	if err := writeText(filepath.Join(root, "sandbox-grants.json"), `{"schemaVersion":99}`); err != nil {
		t.Fatalf("write malformed grants: %v", err)
	}
	if _, err := store.List(); err == nil || !strings.Contains(err.Error(), "unsupported schemaVersion") {
		t.Fatalf("expected unsupported schema error, got %v", err)
	}
}

func TestGrantStoreInvalidatesUnsafeLegacyCommandPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sandbox-grants.json")
	if err := writeText(path, `{"schemaVersion":2,"grants":{},"commandPrefixes":{"bash":[{"toolName":"bash","prefix":["find"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`); err != nil {
		t.Fatalf("write malformed command prefix: %v", err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewGrantStore returned error: %v", err)
	}
	if prefixes, err := store.ListCommandPrefixes(); err != nil || len(prefixes) != 0 {
		t.Fatalf("unsafe legacy prefixes = %#v err=%v, want invalidated", prefixes, err)
	}
	if notice, err := store.ConsumeMigrationNotice(); err != nil || !strings.Contains(notice, "invalidated 1") {
		t.Fatalf("migration notice = %q err=%v", notice, err)
	}
}

func TestGrantStoreInvalidatesMalformedLegacyToolKeys(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sandbox-grants.json")
	original := `{"schemaVersion":2,"grants":{"../escape":[{"toolName":"../escape","decision":"deny","approvedAt":"2026-06-05T14:30:00Z"}]},"commandPrefixes":{"bad name":[{"toolName":"bad name","prefix":["git","status"],"approvedAt":"2026-06-05T14:30:00Z"}]}}`
	if err := writeText(path, original); err != nil {
		t.Fatal(err)
	}
	store, err := NewGrantStore(StoreOptions{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if grants, err := store.List(); err != nil || len(grants) != 0 {
		t.Fatalf("malformed legacy grants = %#v err=%v, want invalidated", grants, err)
	}
	if prefixes, err := store.ListCommandPrefixes(); err != nil || len(prefixes) != 0 {
		t.Fatalf("malformed legacy prefixes = %#v err=%v, want invalidated", prefixes, err)
	}
	if notice, err := store.ConsumeMigrationNotice(); err != nil || !strings.Contains(notice, "invalidated 2") {
		t.Fatalf("migration notice = %q err=%v", notice, err)
	}
	backup, err := os.ReadFile(path + ".v2.backup")
	if err != nil || string(backup) != original {
		t.Fatalf("backup = %q err=%v, want original", backup, err)
	}
}

func TestResolveGrantPathUsesOverrideAndConfigHome(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom.json")
	path, err := ResolveGrantPath(map[string]string{"ZERO_SANDBOX_GRANTS_PATH": override})
	if err != nil {
		t.Fatalf("ResolveGrantPath override returned error: %v", err)
	}
	if path != filepath.Clean(override) {
		t.Fatalf("override path = %q, want %q", path, filepath.Clean(override))
	}

	configHome := t.TempDir()
	path, err = ResolveGrantPath(map[string]string{"XDG_CONFIG_HOME": configHome})
	if err != nil {
		t.Fatalf("ResolveGrantPath config home returned error: %v", err)
	}
	want := filepath.Join(configHome, "zero", "sandbox-grants.json")
	if path != want {
		t.Fatalf("config path = %q, want %q", path, want)
	}
}

func TestFormatGrantList(t *testing.T) {
	empty := FormatGrantList(nil)
	if !strings.Contains(empty, "No persistent sandbox grants") {
		t.Fatalf("unexpected empty list text: %q", empty)
	}
	text := FormatGrantList([]Grant{{
		ToolName:   "write_file",
		Decision:   GrantAllow,
		ApprovedAt: "2026-06-05T14:30:00Z",
		Reason:     "workspace edits",
	}})
	for _, want := range []string{"Sandbox Grants:", "write_file", "allow", "workspace edits"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in formatted grants: %q", want, text)
		}
	}
}

func writeText(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestGrantStoreRevokePathRemovesOnlyMatchingScope(t *testing.T) {
	store, err := NewGrantStore(StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "sandbox-grants.json"),
		Now:      fixedSandboxTime("2026-06-05T14:30:00Z"),
	})
	if err != nil {
		t.Fatalf("NewGrantStore: %v", err)
	}
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	for _, scope := range []string{fileA, fileB} {
		if _, err := store.Grant(GrantInput{ToolName: "write_file", Decision: GrantAllow, Scope: scope, ScopeKind: ScopeFile}); err != nil {
			t.Fatalf("Grant %s: %v", scope, err)
		}
	}
	// A tool-wide grant for the same tool must survive a path-scoped revoke.
	if _, err := store.Grant(GrantInput{ToolName: "write_file", Decision: GrantAllow}); err != nil {
		t.Fatalf("Grant tool-wide: %v", err)
	}

	removed, err := store.RevokePath("write_file", fileA)
	if err != nil || removed != 1 {
		t.Fatalf("RevokePath(fileA) = (%d,%v), want (1,nil)", removed, err)
	}
	grants, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants left (fileB + tool-wide), got %d: %#v", len(grants), grants)
	}
	for _, grant := range grants {
		if grant.Scope == fileA {
			t.Fatalf("fileA grant should have been revoked: %#v", grants)
		}
	}
	// A path with no matching grant removes nothing (and does not error).
	if removed, err := store.RevokePath("write_file", filepath.Join(dir, "nope.txt")); err != nil || removed != 0 {
		t.Fatalf("RevokePath(nonexistent) = (%d,%v), want (0,nil)", removed, err)
	}
}
