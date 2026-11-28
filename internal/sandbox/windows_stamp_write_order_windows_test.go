//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// A READER THAT CANNOT BE RESOLVED MUST NOT COST THE PREVIOUS ATTESTATION.
//
// The stamp is opened with FILE_OVERWRITE_IF, which truncates at open time. The
// reader used to be resolved after that open, so a failure there returned an
// error with the previous run's stamp already emptied, or with a new empty one
// left behind. The directory handle here lacks READ_CONTROL, so the owner lookup
// the reader falls back to is refused, which is the failure being staged.
func TestStampWriterTouchesNothingWhenTheReaderCannotBeResolved(t *testing.T) {
	restore := setWindowsSetupConsumerSID(nil)
	defer restore()

	for _, tc := range []struct {
		name  string
		prior string
	}{
		{name: "a previous stamp", prior: "previous-plan"},
		{name: "no previous stamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			name := windowsSandboxRuntimeStampName("planhash")
			stamp := filepath.Join(root, name)
			if tc.prior != "" {
				if err := os.WriteFile(stamp, []byte(tc.prior), 0o600); err != nil {
					t.Fatalf("write the previous stamp: %v", err)
				}
			}
			path, err := windows.UTF16PtrFromString(root)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := windows.CreateFile(
				path,
				windows.FILE_TRAVERSE|windowsFileAddFile|windows.SYNCHRONIZE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				nil,
				windows.OPEN_EXISTING,
				windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
				0,
			)
			if err != nil {
				t.Fatalf("open the runtime root: %v", err)
			}
			defer windows.CloseHandle(directory)

			err = writeWindowsRuntimeStampToDirectoryHandle(directory, name, "planhash")
			// The failure has to be the reader's. A write refused earlier, at the
			// open, never truncates anything, and would pass this test against the
			// old ordering too.
			if err == nil || !strings.Contains(err.Error(), "read the sandbox runtime root owner") {
				t.Fatalf("SETUP INVALID: the staged reader failure did not happen, got %v", err)
			}

			data, readErr := os.ReadFile(stamp)
			if tc.prior == "" {
				if readErr == nil {
					t.Fatalf("a failed stamp write left a new %d-byte stamp behind (%v)", len(data), err)
				}
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("read the stamp: %v", readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("the previous stamp is gone after a failed write: %v (%v)", readErr, err)
			}
			if string(data) != tc.prior {
				t.Fatalf("a failed stamp write left the previous stamp reading %q, want %q (%v)", data, tc.prior, err)
			}
		})
	}
}
