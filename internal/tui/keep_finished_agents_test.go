package tui

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// KEEP-FINISHED-AGENTS IS A STANDING PREFERENCE, defaulting to the drop.
//
// Dropping finished sub-agents 1.5s after they complete is the product default
// (PR #345). The panel has always had a per-session click-toggle, but it reset
// every session. This is the config lever that makes "keep them" stick.
func TestKeepFinishedAgentsDefaultsToDrop(t *testing.T) {
	if (config.PreferencesConfig{}).KeepsFinishedAgents() {
		t.Fatal("an unset preference keeps agents; the established default is to drop them")
	}
	yes := true
	if !(config.PreferencesConfig{KeepFinishedAgents: &yes}).KeepsFinishedAgents() {
		t.Fatal("an explicit true does not keep them")
	}
	no := false
	if (config.PreferencesConfig{KeepFinishedAgents: &no}).KeepsFinishedAgents() {
		t.Fatal("an explicit false keeps them")
	}
}

// The preference SEEDS showDoneAgents, so finished agents stay from the first
// render without a click.
//
// THROUGH THE REAL CONSTRUCTOR. An earlier version hand-built a model literal,
// which is trivially true and proves nothing about the newModel wiring — a
// mutation deleting that line passed it. This drives newModel(options).
func TestTheKeepPreferenceSeedsTheAgentsPanel(t *testing.T) {
	on := newModel(context.Background(), Options{KeepFinishedAgents: true})
	if !on.showDoneAgents {
		t.Fatal("KeepFinishedAgents=true did not seed showDoneAgents through newModel")
	}
	off := newModel(context.Background(), Options{KeepFinishedAgents: false})
	if off.showDoneAgents {
		t.Fatal("an unset preference seeded the panel on through newModel")
	}
}

// The click-toggle round-trips through the WRITER, so a UI choice reaches disk
// and reads back.
//
// That is half the contract. Surviving a restart also needs the key to survive
// config.Resolve, which this never calls, so it stayed green while mergeConfig
// silently dropped the preference on every launch. The other half is pinned by
// TestKeepFinishedAgentsSurvivesResolve in internal/config, next to the merge it
// is about.
func TestTogglingKeepFinishedAgentsPersists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"

	// READ BACK INDEPENDENTLY after each toggle. Calling the setter again to
	// "reload" proved nothing: it sets the requested value on the config it
	// returns, so both checks passed even when nothing reached the disk.
	onDisk := func() config.FileConfig {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back %s: %v", path, err)
		}
		var cfg config.FileConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return cfg
	}

	if _, err := config.SetKeepFinishedAgents(path, true); err != nil {
		t.Fatal(err)
	}
	if stored := onDisk().Preferences.KeepFinishedAgents; stored == nil || !*stored {
		t.Fatalf("turning 'keep' on did not reach the file: %v", stored)
	}

	if _, err := config.SetKeepFinishedAgents(path, false); err != nil {
		t.Fatal(err)
	}
	if stored := onDisk().Preferences.KeepFinishedAgents; stored == nil || *stored {
		t.Fatalf("turning 'keep' back off did not reach the file: %v", stored)
	}
}
