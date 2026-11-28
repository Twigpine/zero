package cli

import (
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

// Registration used to consult the resolved mode and then, on any other mode,
// the raw --full-auto flag and --auto high again. Those raw flags are already
// folded into the resolved mode when they win. When an explicit
// --permission-mode wins instead, reading them a second time registered Task
// and swarm tools for a run that had resolved to auto or ask.
func TestSpecialistRegistrationFollowsResolvedMode(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options execOptions
		want    bool
	}{
		{name: "full-auto flag", options: execOptions{skipPermissionsUnsafe: true}, want: true},
		{name: "auto high", options: execOptions{autonomy: "high"}, want: true},
		{name: "explicit full-auto", options: execOptions{permissionMode: "full-auto"}, want: true},
		{name: "medium", options: execOptions{autonomy: "medium"}, want: true},
		{name: "low", options: execOptions{autonomy: "low"}, want: false},
		{
			name:    "explicit auto beats the full-auto flag",
			options: execOptions{permissionMode: "auto", skipPermissionsUnsafe: true},
			want:    false,
		},
		{
			name:    "explicit ask beats auto high",
			options: execOptions{permissionMode: "ask", autonomy: "high"},
			want:    false,
		},
		{
			name:    "medium stays registered beside an explicit ask",
			options: execOptions{permissionMode: "ask", autonomy: "medium"},
			want:    true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resolved, err := resolveExecPermissionMode(testCase.options)
			if err != nil {
				t.Fatalf("resolveExecPermissionMode: %v", err)
			}
			if got := shouldRegisterExecSpecialistTools(testCase.options, resolved); got != testCase.want {
				t.Fatalf("register = %v, want %v (resolved %s)", got, testCase.want, resolved)
			}
			if testCase.want && resolved != agent.PermissionModeFullAuto && testCase.options.autonomy != "medium" {
				t.Fatalf("registered without full-auto or medium autonomy (resolved %s)", resolved)
			}
		})
	}
}
