//go:build windows

package planmode

import "testing"

func TestNtObjectPathDriveAndUNC(t *testing.T) {
	// Drive-letter form: `\??\` + absolute path.
	got := ntObjectPath(`C:\Users\example\AppData\Roaming`)
	want := `\??\C:\Users\example\AppData\Roaming`
	if got != want {
		t.Fatalf("drive path = %q, want %q", got, want)
	}

	// UNC form must go through the UNC device, not `\??\\\server\...`.
	got = ntObjectPath(`\\server\share\AppData\Roaming`)
	want = `\??\UNC\server\share\AppData\Roaming`
	if got != want {
		t.Fatalf("UNC path = %q, want %q", got, want)
	}

	// Already-trimmed leading slashes must not produce a double UNC prefix
	// when only one leading pair is present.
	got = ntObjectPath(`\\fileserver\profiles\user`)
	if !hasPrefix(got, `\??\UNC\`) {
		t.Fatalf("UNC path missing UNC device prefix: %q", got)
	}
	if hasPrefix(got, `\??\UNC\\`) {
		t.Fatalf("UNC path has doubled separators: %q", got)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
