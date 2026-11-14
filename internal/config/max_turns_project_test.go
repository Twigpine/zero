package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// PROJECT CONFIG MAY ONLY TIGHTEN THE TURN BUDGET.
//
// A cloned repo setting maxTurns to the ceiling raises the per-run cost for
// whoever opens it — the same hazard PlanSize and DisableZeromaxing are
// tighten-only for. User config stays free to raise: it is the user's own file.
func TestProjectConfigMayLowerTheTurnBudgetButNeverRaiseIt(t *testing.T) {
	for name, tc := range map[string]struct {
		user, project, want int
	}{
		"cannot raise a user value":     {user: 80, project: 500, want: 80},
		"may lower a user value":        {user: 80, project: 50, want: 50},
		"cannot raise from the default": {user: 0, project: 500, want: defaultMaxTurns},
		"may lower below the default":   {user: 0, project: 50, want: 50},
		"equal changes nothing":         {user: 80, project: 80, want: 80},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			userPath := filepath.Join(dir, "user.json")
			projectPath := filepath.Join(dir, "project.json")
			userBody := `{"providers":[{"name":"p","provider_kind":"openai-compatible","catalogID":"xai",` +
				`"baseURL":"https://api.x.ai/v1","apiFormat":"chat-completions","model":"m","apiKey":"k"}],` +
				`"activeProvider":"p"`
			if tc.user > 0 {
				userBody += `,"maxTurns":` + strconv.Itoa(tc.user)
			}
			userBody += `}`
			if err := os.WriteFile(userPath, []byte(userBody), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(projectPath, []byte(`{"maxTurns":`+strconv.Itoa(tc.project)+`}`), 0o600); err != nil {
				t.Fatal(err)
			}

			resolved, err := Resolve(ResolveOptions{UserConfigPath: userPath, ProjectConfigPath: projectPath})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolved.MaxTurns != tc.want {
				t.Errorf("maxTurns = %d, want %d", resolved.MaxTurns, tc.want)
			}
		})
	}
}

// ZERO MEANS "THE DEFAULT", NOT "NO LIMIT", and the distinction is the whole
// defect: the fallback to defaultMaxTurns happens AFTER the project merge, so
// treating an unset user value as unbounded lets a repo raise 80 to 500 for
// every user who never wrote a config — the common case, and the one this rule
// exists for.
func TestAnUnsetUserBudgetIsNotTreatedAsUnbounded(t *testing.T) {
	dst := FileConfig{}
	if err := mergeProjectConfig(&dst, FileConfig{MaxTurns: 500}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if dst.MaxTurns != 0 {
		t.Fatalf("a project raised the budget to %d against an unset user value; it must stay 0 so the default applies", dst.MaxTurns)
	}
}

// The user's own file may still raise it — higher trust, their own money.
func TestUserConfigMayStillRaiseTheTurnBudget(t *testing.T) {
	dst := FileConfig{MaxTurns: 80}
	mergeConfig(&dst, FileConfig{MaxTurns: 300})
	if dst.MaxTurns != 300 {
		t.Errorf("user config could not raise its own budget: %d", dst.MaxTurns)
	}
}
