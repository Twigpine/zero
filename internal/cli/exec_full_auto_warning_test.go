package cli

import "testing"

// The warning explains why approvals are being skipped, so misattributing it
// sends someone looking for a flag they never typed. Attribution used to be a
// partial list of --permission-mode spellings, and full_auto was missing from
// it, so that run was told --auto high was responsible.
func TestFullAutoWarningNamesTheFlagThatWasActuallyPassed(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options execOptions
		want    string
	}{
		{name: "canonical mode", options: execOptions{permissionMode: "full-auto"}, want: "--permission-mode full-auto"},
		{name: "underscore alias", options: execOptions{permissionMode: "full_auto"}, want: "--permission-mode full_auto"},
		{name: "legacy unsafe alias", options: execOptions{permissionMode: "unsafe"}, want: "--permission-mode unsafe"},
		{name: "legacy high alias", options: execOptions{permissionMode: "high"}, want: "--permission-mode high"},
		{name: "boolean flag", options: execOptions{skipPermissionsUnsafe: true}, want: "--full-auto (or --skip-permissions-unsafe)"},
		{name: "autonomy", options: execOptions{autonomy: "high"}, want: "--auto high"},
		{
			// resolveExecPermissionMode reads --permission-mode first, so the
			// attribution has to as well.
			name:    "explicit mode wins over the boolean flag",
			options: execOptions{permissionMode: "full-auto", skipPermissionsUnsafe: true},
			want:    "--permission-mode full-auto",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := fullAutoWarningSource(testCase.options); got != testCase.want {
				t.Errorf("fullAutoWarningSource = %q, want %q", got, testCase.want)
			}
		})
	}
}
