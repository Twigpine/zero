//go:build windows

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyAlreadyCurrentRefusesUnresolvedRecoveryState(t *testing.T) {
	for _, setup := range []struct {
		name string
		run  func(string) error
	}{
		{"relocated recovery", func(target string) error {
			return os.WriteFile(target+".old.deadbeef.recovery", []byte("known-good"), 0o600)
		}},
		{"marked recovery", func(target string) error {
			if err := os.WriteFile(target+".old", []byte("known-good"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(target+".old.keep", []byte("preserve"), 0o600)
		}},
		{"missing target", func(target string) error {
			if err := os.Remove(target); err != nil {
				return err
			}
			return os.WriteFile(target+".old", []byte("known-good"), 0o600)
		}},
	} {
		t.Run(setup.name, func(t *testing.T) {
			dir := t.TempDir()
			targetPath := filepath.Join(dir, "zero.exe")
			if err := os.WriteFile(targetPath, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := setup.run(targetPath); err != nil {
				t.Fatal(err)
			}
			originalExecutable := currentExecutable
			currentExecutable = func() (string, error) { return targetPath, nil }
			defer func() { currentExecutable = originalExecutable }()

			_, err := Apply(context.Background(), Options{
				CurrentVersion: "1.0.0",
				Fetch: func(context.Context, string) (Release, error) {
					return releaseForTarget(t, "v1.0.0", "windows", "amd64"), nil
				},
			})
			if !errors.Is(err, ErrTargetPossiblyTampered) {
				t.Fatalf("Apply error = %v, want ErrTargetPossiblyTampered", err)
			}
		})
	}
}
