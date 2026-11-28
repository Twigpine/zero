//go:build windows

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// testedAttributeBits are the attributes these tests make claims about. A
// directory can hand a new file others, compression above all, and whether the
// machine's temporary directory does that is not what is being tested.
const testedAttributeBits = windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED | windows.FILE_ATTRIBUTE_READONLY |
	windows.FILE_ATTRIBUTE_TEMPORARY | windows.FILE_ATTRIBUTE_ARCHIVE

// testFileAttributes returns the attributes under test that the file has.
func testFileAttributes(t *testing.T, path string) uint32 {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		t.Fatalf("read the attributes of %s: %v", path, err)
	}
	return attributes & testedAttributeBits
}

// newPlainTestFile writes the file an overwrite will replace and clears the
// attributes under test on it. A new file takes the not-content-indexed
// attribute from its directory, and the temporary directory of the machine
// running this may have it, so the starting state is set and not assumed.
func newPlainTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("observed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestFileAttributes(t, path, windows.FILE_ATTRIBUTE_ARCHIVE)
	if got := testFileAttributes(t, path); got != windows.FILE_ATTRIBUTE_ARCHIVE {
		t.Fatalf("SETUP INVALID: attributes = %#x after they were cleared", got)
	}
}

func setTestFileAttributes(t *testing.T, path string, attributes uint32) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(name, attributes); err != nil {
		t.Fatalf("set the attributes of %s: %v", path, err)
	}
}

// AN OVERWRITE KEEPS THE ATTRIBUTES OF THE FILE IT REPLACES.
//
// Publishing an overwrite by rename replaced the file with a new one that had
// no attributes beyond the archive bit, so a hidden file came back visible and
// a system file lost its mark. Both tools that publish through the rename are
// checked, on both ways a side file is created: with the replaced file's
// access control, and on a volume that keeps none.
func TestOverwriteKeepsTheReplacedFilesAttributes(t *testing.T) {
	for _, layout := range []struct {
		name       string
		attributes uint32
	}{
		{name: "hidden", attributes: windows.FILE_ATTRIBUTE_HIDDEN},
		{name: "system", attributes: windows.FILE_ATTRIBUTE_SYSTEM},
		{name: "hidden and system", attributes: windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM},
		{name: "not content indexed", attributes: windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED},
	} {
		for _, volume := range []struct {
			name     string
			keepsACL bool
		}{
			{name: "a volume with access control lists", keepsACL: true},
			{name: "a volume without them", keepsACL: false},
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
				t.Run(layout.name+"/"+volume.name+"/"+tool.name, func(t *testing.T) {
					if !volume.keepsACL {
						prior := fileVolumeKeepsACLs
						fileVolumeKeepsACLs = func(windows.Handle) (bool, error) { return false, nil }
						t.Cleanup(func() { fileVolumeKeepsACLs = prior })
					}
					root := t.TempDir()
					path := filepath.Join(root, "kept.txt")
					newPlainTestFile(t, path)
					setTestFileAttributes(t, path, layout.attributes)
					before := testFileAttributes(t, path)
					if before&layout.attributes != layout.attributes {
						t.Fatalf("SETUP INVALID: attributes = %#x after setting %#x", before, layout.attributes)
					}

					result := tool.run(root)
					if result.Status != StatusOK {
						t.Fatalf("%s = %s: %s", tool.name, result.Status, result.Output)
					}
					content, err := os.ReadFile(path)
					if err != nil || string(content) != "replacement\n" {
						t.Fatalf("content = %q, err=%v; the overwrite did not land", content, err)
					}
					// A modified file has the archive bit; nothing else may differ.
					want := before | windows.FILE_ATTRIBUTE_ARCHIVE
					if after := testFileAttributes(t, path); after != want {
						t.Fatalf("attributes across the overwrite: %#x before, %#x after, want %#x", before, after, want)
					}
				})
			}
		}
	}
}

// ONLY THE PRESENTATION ATTRIBUTES ARE CARRIED.
//
// The carry adds to what the new file has. It must not copy bits that describe
// how the old file object was stored, which cannot be set as attributes and
// would make the call fail or lie, and a file with nothing to carry must come
// out as an ordinary new file.
func TestOverwriteCarriesNothingBeyondThePresentationAttributes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "plain.txt")
	newPlainTestFile(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	carried := carriedFileAttributes(info)
	if carried == nil {
		t.Fatal("an existing file carries nil; it must carry the fact that none of the attributes is set")
	}
	if *carried != 0 {
		t.Fatalf("an ordinary file carries %#x, want none of the attributes", *carried)
	}
	var handed []uint32
	installFileWriteAttributes(t, func(file *os.File, attributes *uint32) error {
		if attributes == nil {
			t.Error("an overwrite handed its side file nil in place of the replaced file's attributes")
			return nil
		}
		handed = append(handed, *attributes)
		return applyFileAttributes(file, attributes)
	})
	setTestFileAttributes(t, path, windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_ARCHIVE|windows.FILE_ATTRIBUTE_TEMPORARY)
	if before := testFileAttributes(t, path); before&windows.FILE_ATTRIBUTE_TEMPORARY == 0 {
		t.Fatalf("SETUP INVALID: attributes = %#x, the temporary bit did not take", before)
	}

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "plain.txt", "content": "replacement\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", result.Status, result.Output)
	}
	if len(handed) != 1 || handed[0] != windows.FILE_ATTRIBUTE_HIDDEN {
		t.Fatalf("the side file was handed %#x, want the hidden bit alone", handed)
	}
	if after := testFileAttributes(t, path); after != windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_ARCHIVE {
		t.Fatalf("attributes after the overwrite = %#x, want hidden and archive", after)
	}
}

// AN OVERWRITE DOES NOT PICK UP THE DIRECTORY'S ATTRIBUTES.
//
// A new file inherits the not-content-indexed attribute from its directory.
// The side file is a new file, so in a directory that has the attribute it is
// born with it, and the file it replaces may not have had it. The carry sets
// the presentation attributes to exactly what the replaced file had, which
// means clearing what the directory handed the side file as well as adding
// what the original had.
func TestOverwriteDoesNotPickUpTheDirectorysAttributes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "kept.txt")
	newPlainTestFile(t, path)
	setTestFileAttributes(t, root, windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED)
	probe := filepath.Join(root, "probe.txt")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if testFileAttributes(t, probe)&windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED == 0 {
		t.Skip("this volume does not hand the directory's not-content-indexed attribute to a new file")
	}
	before := testFileAttributes(t, path)
	if before&windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED != 0 {
		t.Fatalf("SETUP INVALID: the existing file already has the attribute: %#x", before)
	}

	result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
		"path": "kept.txt", "content": "replacement\n", "overwrite": true,
	})
	if result.Status != StatusOK {
		t.Fatalf("overwrite = %s: %s", result.Status, result.Output)
	}
	want := before | windows.FILE_ATTRIBUTE_ARCHIVE
	if after := testFileAttributes(t, path); after != want {
		t.Fatalf("attributes across the overwrite: %#x before, %#x after, want %#x; the directory's attribute leaked onto the file", before, after, want)
	}
}

// THE LAYER THAT WRITES ATTRIBUTES ENFORCES THE MASK ITSELF.
//
// What the commit path hands it is already masked, so an overwrite cannot show
// whether this layer would pass other bits through. It is called directly here
// with bits it must not set.
func TestApplyFileAttributesSetsOnlyThePresentationAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staged.txt")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	bits := uint32(windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_TEMPORARY | windows.FILE_ATTRIBUTE_READONLY)
	applyErr := applyFileAttributes(file, &bits)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if applyErr != nil {
		t.Fatalf("applyFileAttributes: %v", applyErr)
	}
	after := testFileAttributes(t, path)
	if after&windows.FILE_ATTRIBUTE_HIDDEN == 0 {
		t.Fatalf("SETUP INVALID: attributes = %#x, the hidden bit was not applied", after)
	}
	if after&(windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_ATTRIBUTE_READONLY) != 0 {
		t.Fatalf("attributes = %#x; only the presentation attributes may be set from the carried value", after)
	}
}
