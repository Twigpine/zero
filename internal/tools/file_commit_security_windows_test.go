//go:build windows

package tools

import (
	"context"
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
