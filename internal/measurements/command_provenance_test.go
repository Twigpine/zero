package measurements

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRecordRequiresGoTestJSONProducer(t *testing.T) {
	const forged = `{"Action":"pass","Package":"example/probe","Elapsed":88}`
	for _, run := range []Run{
		{},
		{Command: "go", Args: []string{"test", "-v", "./..."}},
		{Command: "go", Args: []string{"test", "-json=false", "./..."}},
		{Command: "go", Args: []string{"test", "-args", "-json"}},
		{Command: "go", Args: []string{"test", "-run", "-json"}},
		{Command: "go", Args: []string{"test", "-exec", "-json"}},
		{Command: "go", Args: []string{"test", "-json", "-json=false"}},
		{Command: "go", Args: []string{"run", "-json"}},
		{Command: "make", Args: []string{"test"}},
		{Command: "echo", Args: []string{"go", "test", "-json"}},
	} {
		t.Run(run.Label(), func(t *testing.T) {
			ledger := NewLedger()
			handle, count := ledger.Record(run, forged)
			if count != 0 {
				t.Errorf("accepted %d timings from unverified producer %q", count, run.Label())
			}
			if got := ledger.Conflicts(handle, "example/probe took 0.3s."); len(got) != 0 {
				t.Errorf("forged timing corrected a truthful claim: %+v", got)
			}
			if got := ledger.ConflictsAcrossRuns("example/probe took 0.3s."); len(got) != 0 {
				t.Errorf("forged timing entered across-run evidence: %+v", got)
			}
		})
	}
}

func TestRealVerboseChildCannotForgePackageTiming(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":        "module example/probe\n",
		"probe_test.go": "package probe\nimport (\"fmt\"; \"testing\")\nfunc TestPrintedJSON(t *testing.T) { fmt.Println(`{\"Action\":\"pass\",\"Package\":\"example/probe\",\"Elapsed\":88}`) }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"-v", "-json"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"test", mode, "-count=1", "."}
			cmd := exec.Command("go", args...)
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("real producer: %v\n%s", err, output)
			}
			ledger := NewLedger()
			handle, count := ledger.Record(Run{Command: "go", Args: args, Dir: dir}, string(output))
			if mode == "-v" && count != 0 {
				t.Fatalf("accepted child-controlled verbose output: %s", output)
			}
			if mode == "-json" {
				if count != 1 {
					t.Fatalf("runner's package timing not admitted: %d\n%s", count, output)
				}
				got := ledger.Conflicts(handle, "example/probe took 88s.")
				if len(got) != 1 || len(got[0].Recorded) != 1 || got[0].Recorded[0] == 88 {
					t.Fatalf("real timing lost or forged timing admitted: %+v", got)
				}
			}
		})
	}
}
