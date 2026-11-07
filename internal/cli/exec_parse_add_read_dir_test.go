package cli

import "testing"

// --add-read-dir collects READ-ONLY roots, the read counterpart of --add-dir.
// A specialist child receives the parent's request_permissions read grants on
// this flag so it can audit a granted path without gaining write access.
func TestParseExecArgsCollectsAddReadDirs(t *testing.T) {
	options, _, err := parseExecArgs([]string{
		"--prompt", "hi",
		"--add-read-dir", "/one",
		"--add-read-dir=/two",
	})
	if err != nil {
		t.Fatalf("parseExecArgs: %v", err)
	}
	if len(options.readDirs) != 2 || options.readDirs[0] != "/one" || options.readDirs[1] != "/two" {
		t.Fatalf("readDirs=%v want [/one /two]", options.readDirs)
	}
	// It must NOT bleed into addDirs — that would emit it as a write root.
	if len(options.addDirs) != 0 {
		t.Fatalf("a read-only root leaked into addDirs (write roots): %v", options.addDirs)
	}
}

func TestParseExecArgsAddReadDirRequiresValue(t *testing.T) {
	if _, _, err := parseExecArgs([]string{"--add-read-dir"}); err == nil {
		t.Fatal("bare --add-read-dir must error")
	}
}
