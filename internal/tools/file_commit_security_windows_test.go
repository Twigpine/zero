//go:build windows

package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func currentUserSIDString(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("read the current user: %v", err)
	}
	return user.User.Sid.String()
}

func setTestFileDACL(t *testing.T, path string, sddl string, protected bool) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse %q: %v", sddl, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("decode %q: %v", sddl, err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		information |= windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION)
	} else {
		information |= windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		t.Fatalf("set the access control list on %s: %v", path, err)
	}
}

// renderedTestDACL returns sddl the way a list read back from a file is
// printed. The printed form names some accounts by a two-letter alias in place
// of the identifier they were written with: the built-in administrator account
// a hosted Windows runner uses comes back as LA. An expectation built from the
// raw identifier therefore matches on a developer's machine and not there.
func renderedTestDACL(t *testing.T, sddl string) string {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse %q: %v", sddl, err)
	}
	return descriptor.String()
}

// testDACLEntries returns the entries of a printed list without the flags in
// front of them, which say how the list was set and not who it admits.
func testDACLEntries(sddl string) string {
	start := strings.Index(sddl, "(")
	if start < 0 {
		return ""
	}
	return sddl[start:]
}

// testFileDACL returns the file's access control list as SDDL, and whether it
// is protected from inheritance.
func testFileDACL(t *testing.T, path string) (string, bool) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the access control list of %s: %v", path, err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read the access control flags of %s: %v", path, err)
	}
	return descriptor.String(), control&windows.SE_DACL_PROTECTED != 0
}

// AN OVERWRITE KEEPS THE ACCESS CONTROL OF THE FILE IT REPLACES.
//
// Publishing an overwrite by rename replaced the file with one that had only
// the directory's inherited access control. A file with an extra entry lost
// it, and a file whose owner had removed inheritance and kept access to
// themselves came back readable by everyone the directory allows. Both tools
// that publish through the rename are checked, against the list read back from
// the file itself.
func TestOverwriteKeepsTheReplacedFilesAccessControl(t *testing.T) {
	me := currentUserSIDString(t)
	for _, layout := range []struct {
		name      string
		sddl      string
		protected bool
	}{
		{name: "an explicit entry beside the inherited ones", sddl: "D:(A;;FA;;;" + me + ")(A;;FR;;;WD)"},
		{name: "inheritance removed, owner only", sddl: "D:P(A;;FA;;;" + me + ")", protected: true},
	} {
		for _, tool := range []struct {
			name string
			run  func(root string) Result
		}{
			{name: "write_file", run: func(root string) Result {
				return NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
					"path": "kept.txt", "content": "replacement\n", "overwrite": true,
				})
			}},
			{name: "edit_file", run: func(root string) Result {
				return NewScopedEditFileTool(root, nil).Run(context.Background(), map[string]any{
					"path": "kept.txt", "old_string": "observed", "new_string": "replacement",
				})
			}},
		} {
			t.Run(layout.name+"/"+tool.name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "kept.txt")
				if err := os.WriteFile(path, []byte("observed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				setTestFileDACL(t, path, layout.sddl, layout.protected)
				before, protectedBefore := testFileDACL(t, path)
				if protectedBefore != layout.protected {
					t.Fatalf("SETUP INVALID: protected = %v after setting %q", protectedBefore, layout.sddl)
				}
				if !layout.protected && !strings.Contains(before, "(A;;FR;;;WD)") {
					t.Fatalf("SETUP INVALID: the explicit entry is missing from %q", before)
				}

				result := tool.run(root)
				if result.Status != StatusOK {
					t.Fatalf("%s = %s: %s", tool.name, result.Status, result.Output)
				}
				content, err := os.ReadFile(path)
				if err != nil || string(content) != "replacement\n" {
					t.Fatalf("content = %q, err=%v; the overwrite did not land", content, err)
				}
				after, protectedAfter := testFileDACL(t, path)
				if after != before || protectedAfter != protectedBefore {
					t.Fatalf("access control changed across the overwrite:\n before: %s (protected=%v)\n  after: %s (protected=%v)",
						before, protectedBefore, after, protectedAfter)
				}
			})
		}
	}
}

// openFileDACL returns the access control list of an open file as SDDL, and
// whether it is protected from inheritance, without going through its name.
func openFileDACL(t *testing.T, file *os.File) (string, bool) {
	t.Helper()
	handle, err := reopenFileForSecurity(file, windows.READ_CONTROL)
	if err != nil {
		t.Fatalf("reopen the staged file: %v", err)
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the staged file's access control list: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read the staged file's access control flags: %v", err)
	}
	return descriptor.String(), control&windows.SE_DACL_PROTECTED != 0
}

// THE REPLACEMENT IS NEVER READABLE BY SOMEONE THE ORIGINAL EXCLUDED.
//
// The side file is born with the directory's access control. If the directory
// lets everyone read and the file being replaced was kept to its owner, the
// replacement bytes must not sit in that side file under the directory's list
// even for a moment. So the original's list goes on while the side file is
// still empty, and this looks at the side file at the instant its bytes are
// written.
func TestStagedBytesAreWrittenUnderTheReplacedFilesAccessControl(t *testing.T) {
	me := currentUserSIDString(t)
	root := t.TempDir()
	// Everything created in this directory inherits read access for everyone.
	setTestFileDACL(t, root, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FR;;;WD)", true)
	path := filepath.Join(root, "private.txt")
	if err := os.WriteFile(path, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inherited, _ := testFileDACL(t, path)
	if !strings.Contains(inherited, ";;;WD)") {
		t.Fatalf("SETUP INVALID: a file created here does not inherit the everyone entry: %q", inherited)
	}
	setTestFileDACL(t, path, "D:P(A;;FA;;;"+me+")", true)
	want, _ := testFileDACL(t, path)

	writes := 0
	var atWrite string
	var protectedAtWrite bool
	var sizeAtWrite int64
	installFileWriteTempWrite(t, func(file *os.File, content string) error {
		writes++
		atWrite, protectedAtWrite = openFileDACL(t, file)
		if info, err := file.Stat(); err == nil {
			sizeAtWrite = info.Size()
		}
		_, err := io.WriteString(file, content)
		return err
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "private.txt", "content": "replacement\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", result.Status, result.Output)
	}
	if writes != 1 {
		t.Fatalf("SETUP INVALID: the side file write ran %d times, want once", writes)
	}
	if sizeAtWrite != 0 {
		t.Fatalf("SETUP INVALID: the side file already held %d bytes when the write seam ran", sizeAtWrite)
	}
	if strings.Contains(atWrite, ";;;WD)") || !protectedAtWrite || atWrite != want {
		t.Fatalf("the side file was written under the wrong access control:\n at write: %s (protected=%v)\n  original: %s",
			atWrite, protectedAtWrite, want)
	}
}

// NOBODY ELSE CAN OPEN THE SIDE FILE, FROM THE MOMENT IT EXISTS.
//
// Putting the original's list on an empty side file is not enough by itself.
// Access is granted when a file is opened and is not taken back when its list
// changes, so a side file that existed for an instant under the directory's
// list could be opened in that instant and read through that handle later.
// The side file is therefore created admitting the current user alone.
//
// Here the directory and the replaced file both let everyone read, so neither
// inheritance nor the carried list can explain a private side file: only the
// way it was created can. The list is read at the point the original's list is
// about to be applied, which is the earliest the side file can be observed.
func TestOverwriteSideFileIsPrivateFromBirth(t *testing.T) {
	me := currentUserSIDString(t)
	root := t.TempDir()
	setTestFileDACL(t, root, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FR;;;WD)", true)
	path := filepath.Join(root, "shared.txt")
	if err := os.WriteFile(path, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestFileDACL(t, path, "D:P(A;;FA;;;"+me+")(A;;FR;;;WD)", true)
	want, _ := testFileDACL(t, path)
	if !strings.Contains(want, ";;;WD)") {
		t.Fatalf("SETUP INVALID: the replaced file does not admit everyone: %q", want)
	}

	calls := 0
	var atBirth string
	var protectedAtBirth bool
	installFileWriteSecurity(t, nil, func(file *os.File, security *fileSecurity) error {
		calls++
		atBirth, protectedAtBirth = openFileDACL(t, file)
		return applyFileSecurity(file, security)
	})

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "shared.txt", "content": "replacement\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", result.Status, result.Output)
	}
	if calls != 1 {
		t.Fatalf("SETUP INVALID: the access control seam ran %d times, want once", calls)
	}
	private := testDACLEntries(renderedTestDACL(t, "D:P(A;;FA;;;"+me+")"))
	if !protectedAtBirth || testDACLEntries(atBirth) != private {
		t.Fatalf("the side file was not private when it was created: %s (protected=%v); want the one entry %s, for the current user",
			atBirth, protectedAtBirth, private)
	}
	after, _ := testFileDACL(t, path)
	if after != want {
		t.Fatalf("the published file's access control changed:\n before: %s\n  after: %s", want, after)
	}
}

// A VOLUME THAT KEEPS NO ACCESS CONTROL LISTS HAS NOTHING TO CARRY.
//
// FAT and exFAT volumes store none, and asking one to take a list fails. The
// overwrite must still work there, carrying nothing.
func TestOverwriteOnAVolumeWithoutAccessControlListsCarriesNothing(t *testing.T) {
	prior := fileVolumeKeepsACLs
	fileVolumeKeepsACLs = func(windows.Handle) (bool, error) { return false, nil }
	t.Cleanup(func() { fileVolumeKeepsACLs = prior })
	var applied []*fileSecurity
	installFileWriteSecurity(t, nil, func(_ *os.File, security *fileSecurity) error {
		applied = append(applied, security)
		return nil
	})

	root := t.TempDir()
	path := filepath.Join(root, "kept.txt")
	if err := os.WriteFile(path, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "kept.txt", "content": "replacement\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", result.Status, result.Output)
	}
	if len(applied) != 1 || applied[0] != nil {
		t.Fatalf("the side file was handed %v; a volume without access control lists has none to carry", applied)
	}
}
