//go:build windows

package planmode

import (
	"strings"
	"testing"
)

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
	if !strings.HasPrefix(got, `\??\UNC\`) {
		t.Fatalf("UNC path missing UNC device prefix: %q", got)
	}
	if strings.HasPrefix(got, `\??\UNC\\`) {
		t.Fatalf("UNC path has doubled separators: %q", got)
	}

	// Extended-length prefix is not UNC: it must map to `\??\`, not
	// `\??\UNC\?\...`.
	got = ntObjectPath(`\\?\C:\Users\example\AppData\Roaming`)
	want = `\??\C:\Users\example\AppData\Roaming`
	if got != want {
		t.Fatalf("extended-length path = %q, want %q", got, want)
	}

	// Extended-length UNC is already UNC-qualified after stripping `\\?\`.
	got = ntObjectPath(`\\?\UNC\server\share\AppData\Roaming`)
	want = `\??\UNC\server\share\AppData\Roaming`
	if got != want {
		t.Fatalf("extended-length UNC path = %q, want %q", got, want)
	}

	// Device prefix must map to `\??\`, not `\??\UNC\.\...`.
	got = ntObjectPath(`\\.\C:\Users\example\AppData\Roaming`)
	want = `\??\C:\Users\example\AppData\Roaming`
	if got != want {
		t.Fatalf("device path = %q, want %q", got, want)
	}
}
