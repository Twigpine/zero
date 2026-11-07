package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A restart goes through Resolve, so that is where the persistence has to be
// proven.
//
// The existing coverage (internal/tui/keep_finished_agents_test.go) round-trips
// through SetKeepFinishedAgents and reads the file back, which proves the WRITE.
// It stayed green for the whole life of the bug, because the half that was
// broken is the read on the next launch, and Resolve is the only thing on that
// path. mergeConfig copied FavoriteModels, RecentModels, Recaps and Theme and
// never touched this key, so the value reached disk and died there.
//
// Recaps and Theme are asserted alongside on purpose. They are the siblings this
// field is modelled on, so a future edit that breaks the merge for one of them
// says which.
func TestKeepFinishedAgentsSurvivesResolve(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want bool
	}{
		{
			name: "an explicit keep survives the merge",
			body: `{"preferences":{"keepFinishedAgents":true,"recaps":false,"theme":"dracula"}}`,
			want: true,
		},
		{
			// The reason the field is a pointer: with omitempty an explicit false
			// is indistinguishable from an absent key unless the merge copies the
			// pointer rather than the value.
			name: "an explicit drop survives it too",
			body: `{"preferences":{"keepFinishedAgents":false}}`,
			want: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(testCase.body), 0o600); err != nil {
				t.Fatal(err)
			}
			resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := resolved.Preferences.KeepsFinishedAgents(); got != testCase.want {
				t.Errorf("after Resolve, KeepsFinishedAgents() = %v, want %v; the persisted preference did not survive the merge", got, testCase.want)
			}
			if resolved.Preferences.KeepFinishedAgents == nil {
				t.Error("the merge dropped the pointer, so an explicit false is now indistinguishable from an unset key")
			}
		})
	}
}

// The neighbouring preferences must keep coming through. This is the assertion
// that would have localised the bug: theme and recaps arriving while
// keepFinishedAgents did not is what pointed at mergeConfig rather than at the
// unmarshal or the writer.
func TestNeighbouringPreferencesStillSurviveResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"preferences":{"keepFinishedAgents":true,"recaps":false,"theme":"dracula"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Preferences.Theme != "dracula" {
		t.Errorf("theme = %q, want dracula", resolved.Preferences.Theme)
	}
	if resolved.Preferences.Recaps == nil || *resolved.Preferences.Recaps {
		t.Error("an explicit recaps=false did not survive Resolve")
	}
}

// Absent stays absent. Without this, a merge that assigned the key
// unconditionally would pass the tests above while quietly turning the product
// default into "keep".
func TestKeepFinishedAgentsStaysUnsetWhenTheConfigIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"preferences":{"theme":"dracula"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Preferences.KeepFinishedAgents != nil {
		t.Error("a config that says nothing about the preference resolved to an explicit value")
	}
	if resolved.Preferences.KeepsFinishedAgents() {
		t.Error("the established default is to drop finished agents")
	}
}
