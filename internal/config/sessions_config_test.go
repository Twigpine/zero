package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionsConfigRoundTripsAsAKnownKey(t *testing.T) {
	var cfg FileConfig
	if err := json.Unmarshal([]byte(`{"sessions":{"retentionDays":14}}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Sessions.RetentionDays != 14 {
		t.Fatalf("retentionDays = %d, want 14", cfg.Sessions.RetentionDays)
	}
	if _, stray := cfg.Extra["sessions"]; stray {
		t.Fatal("sessions was kept as an unknown key as well as parsed")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"sessions":{"retentionDays":14}`) {
		t.Fatalf("marshalled config lost the setting: %s", data)
	}
	empty, err := json.Marshal(FileConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "sessions") {
		t.Fatalf("an unset sessions block was written out: %s", empty)
	}
}

func TestSessionsConfigRejectsANegativeRetention(t *testing.T) {
	var cfg FileConfig
	err := json.Unmarshal([]byte(`{"sessions":{"retentionDays":-1}}`), &cfg)
	if err == nil || !strings.Contains(err.Error(), "retentionDays") {
		t.Fatalf("a negative retention was accepted: %v", err)
	}
}

// A write that changes something else must not drop the setting.
func TestSessionsConfigSurvivesAnUnrelatedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"sessions":{"retentionDays":21}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetTheme(path, "dark"); err != nil {
		t.Fatalf("SetTheme: %v", err)
	}
	settings, err := ReadSessionsConfig(path)
	if err != nil {
		t.Fatalf("ReadSessionsConfig: %v", err)
	}
	if settings.RetentionDays != 21 {
		t.Fatalf("after an unrelated write retentionDays = %d, want 21", settings.RetentionDays)
	}
}

func TestReadSessionsConfig(t *testing.T) {
	dir := t.TempDir()
	if settings, err := ReadSessionsConfig(filepath.Join(dir, "missing.json")); err != nil || settings != (SessionsConfig{}) {
		t.Fatalf("a missing config = %+v, %v; want empty and no error", settings, err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"sessions":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionsConfig(broken); err == nil {
		t.Fatal("a config that does not parse was read as empty")
	}
}
