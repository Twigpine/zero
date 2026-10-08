package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/fsutil"
)

const windowsUnelevatedSetupMarkerSchemaVersion = 2

// windowsUnelevatedSetupMarkerMaxPlans bounds the applied-plan history so the
// marker cannot grow without limit when a user hops between many workspaces.
// Oldest entries are dropped first; re-applying an already-granted plan is
// harmless, so eviction only costs a redundant ACL apply.
const windowsUnelevatedSetupMarkerMaxPlans = 64

// WindowsUnelevatedAppliedPlan fingerprints one workspace ACL plan the
// unelevated tier has applied. Unlike the elevated setup marker it carries no
// network infrastructure hash: the unelevated tier never provisions WFP
// filters, so there is nothing network-side to fingerprint.
type WindowsUnelevatedAppliedPlan struct {
	ACLPlanHash    string `json:"aclPlanHash"`
	ACLPlanEntries int    `json:"aclPlanEntries"`
}

// WindowsUnelevatedSetupMarker records which workspace ACL plans the command
// runner has applied without elevation, keyed by plan hash, so repeat commands
// in a known workspace skip the re-apply. One file serves every workspace under
// a sandbox home (the elevated marker is instead a single-plan snapshot,
// because its WFP half genuinely is machine-global).
type WindowsUnelevatedSetupMarker struct {
	SchemaVersion int                            `json:"schemaVersion"`
	AppliedPlans  []WindowsUnelevatedAppliedPlan `json:"appliedPlans,omitempty"`
}

func WindowsUnelevatedSetupMarkerPath(sandboxHome string) string {
	return filepath.Join(filepath.Clean(sandboxHome), "windows-unelevated-setup.json")
}

// buildWindowsUnelevatedAppliedPlan derives the ACL plan for the command's
// permission profile plus its marker fingerprint. The same BuildWindowsACLPlan
// output later feeds the apply step, so the fingerprint and the applied grants
// can never drift apart.
func buildWindowsUnelevatedAppliedPlan(config WindowsSandboxCommandConfig) (WindowsUnelevatedAppliedPlan, WindowsACLPlan, error) {
	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		return WindowsUnelevatedAppliedPlan{}, WindowsACLPlan{}, err
	}
	hash, err := WindowsACLPlanHash(plan)
	if err != nil {
		return WindowsUnelevatedAppliedPlan{}, WindowsACLPlan{}, err
	}
	return WindowsUnelevatedAppliedPlan{
		ACLPlanHash:    hash,
		ACLPlanEntries: len(plan.Entries),
	}, plan, nil
}

// loadWindowsUnelevatedSetupMarker reads the marker; a missing file is the
// common first-run state and returns an empty marker, not an error. A marker
// with an unexpected schema version or a corrupt/truncated body is likewise
// treated as empty so the runner re-applies (and rewrites) rather than failing:
// the marker only memoizes idempotent ACL applies, so resetting it can only
// cost redundant work, never skip enforcement.
func loadWindowsUnelevatedSetupMarker(sandboxHome string) (WindowsUnelevatedSetupMarker, error) {
	sandboxHome = strings.TrimSpace(sandboxHome)
	if sandboxHome == "" {
		return WindowsUnelevatedSetupMarker{}, errors.New("windows sandbox home is required")
	}
	path := WindowsUnelevatedSetupMarkerPath(sandboxHome)
	bytes, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WindowsUnelevatedSetupMarker{SchemaVersion: windowsUnelevatedSetupMarkerSchemaVersion}, nil
		}
		return WindowsUnelevatedSetupMarker{}, fmt.Errorf("read windows unelevated setup marker: %w", err)
	}
	var marker WindowsUnelevatedSetupMarker
	if err := json.Unmarshal(bytes, &marker); err != nil {
		return WindowsUnelevatedSetupMarker{SchemaVersion: windowsUnelevatedSetupMarkerSchemaVersion}, nil
	}
	if marker.SchemaVersion != windowsUnelevatedSetupMarkerSchemaVersion {
		return WindowsUnelevatedSetupMarker{SchemaVersion: windowsUnelevatedSetupMarkerSchemaVersion}, nil
	}
	return marker, nil
}

func (marker WindowsUnelevatedSetupMarker) contains(applied WindowsUnelevatedAppliedPlan) bool {
	if strings.TrimSpace(applied.ACLPlanHash) == "" {
		return false
	}
	for _, plan := range marker.AppliedPlans {
		if plan.ACLPlanHash == applied.ACLPlanHash && plan.ACLPlanEntries == applied.ACLPlanEntries {
			return true
		}
	}
	return false
}

// recordWindowsUnelevatedAppliedPlan appends the plan to the marker (dropping
// the oldest entries past the cap) and writes it atomically, following the
// temp-file-then-rename pattern of the sibling marker writers.
func recordWindowsUnelevatedAppliedPlan(sandboxHome string, applied WindowsUnelevatedAppliedPlan) error {
	marker, err := loadWindowsUnelevatedSetupMarker(sandboxHome)
	if err != nil {
		return err
	}
	if marker.contains(applied) {
		return nil
	}
	marker.SchemaVersion = windowsUnelevatedSetupMarkerSchemaVersion
	marker.AppliedPlans = append(marker.AppliedPlans, applied)
	if overflow := len(marker.AppliedPlans) - windowsUnelevatedSetupMarkerMaxPlans; overflow > 0 {
		marker.AppliedPlans = append([]WindowsUnelevatedAppliedPlan(nil), marker.AppliedPlans[overflow:]...)
	}
	path := WindowsUnelevatedSetupMarkerPath(sandboxHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create windows unelevated setup marker dir: %w", err)
	}
	bytes, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal windows unelevated setup marker: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".windows-unelevated-setup-*.tmp")
	if err != nil {
		return fmt.Errorf("create windows unelevated setup marker temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(bytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write windows unelevated setup marker temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close windows unelevated setup marker temp file: %w", err)
	}
	if err := fsutil.RenameWithRetry(tmpPath, path, nil); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace windows unelevated setup marker: %w", err)
	}
	return nil
}
