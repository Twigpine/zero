package specialist

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/pathjail"
)

// Named plans: a plan that ran once, saved and run again.
//
// A plan is stored as the ARGUMENTS ParsePlan accepts, never as a serialised
// Plan, and it is re-admitted through ParsePlan on load. That is what keeps the
// "no path from stored data to an executable that skipped validation" property:
// a saved plan is re-validated against the CURRENT run's limits, so a plan saved
// when the tier was large is refused on a run where it is small, and a plan
// naming a tool this run does not hold is refused rather than granted.
//
// TWO SCOPES, project shadowing user, mirroring usercommands and the specialist
// loader. A project plan is checked into the repo and shared with the team.
//
// HOW ONE IS RUN, and this is a deliberate divergence from the gap report. The
// report wants saved plans to become `/name` slash commands. They do not: a
// plan is not a prompt, and giving each saved plan its own command would need a
// path from the TUI straight into tool execution — a SECOND way to run a plan,
// beside the model calling orchestrate. Instead the tool takes a `saved`
// argument. One execution path, one set of guards, and the model can compose a
// saved plan into a turn the way it composes anything else.
//
// MALFORMED IS AN ERROR, NEVER A SILENT SKIP (invariant 13). A plan file that
// does not parse is reported by name; dropping it would leave a user believing
// they had run something.

// planFileExt is the stored extension. JSON, not a script: the argument shape is
// already data, and the whole feature exists partly because a script language
// would have needed an evaluator.
const planFileExt = ".json"

// planTempExt is the extension SavePlan's in-progress file carries. Deliberately
// NOT planFileExt: ListPlans matches that exactly, so a temp file a crash left
// behind is never offered as a saved plan.
const planTempExt = ".tmp"

// PlanPaths are the directories scanned for saved plans, project first.
type PlanPaths struct {
	// ProjectRoot and UserRoot are the containment boundaries for the two
	// directories below. Saving is performed relative to a handle on the
	// matching root, so no component of the directory can redirect the write
	// outside it.
	ProjectRoot string
	UserRoot    string
	ProjectDir  string
	UserDir     string
}

// DefaultPlanPaths returns the project and user plan directories.
func DefaultPlanPaths(workspaceRoot, userConfigDir string) PlanPaths {
	var paths PlanPaths
	if strings.TrimSpace(workspaceRoot) != "" {
		paths.ProjectRoot = workspaceRoot
		paths.ProjectDir = filepath.Join(workspaceRoot, ".zero", "plans")
	}
	if strings.TrimSpace(userConfigDir) != "" {
		paths.UserRoot = userConfigDir
		paths.UserDir = filepath.Join(userConfigDir, "zero", "plans")
	}
	return paths
}

// PlanScope says where a saved plan came from. An ENUM rather than the bool it
// started as: there are three origins now, and "not project" would have meant
// both "the user's" and "shipped with the binary" — two things a listing has to
// tell apart.
type PlanScope string

const (
	// PlanScopeBuiltin is bundled with the binary. Always shadowed.
	PlanScopeBuiltin PlanScope = "builtin"
	// PlanScopeUser is the user config directory.
	PlanScopeUser PlanScope = "user"
	// PlanScopeProject is .zero/plans in the workspace, checked in and shared.
	PlanScopeProject PlanScope = "project"
)

// SavedPlan is a stored plan as found on disk. Args is the raw argument map,
// not a Plan: it becomes a Plan only by going through ParsePlan.
type SavedPlan struct {
	Name        string
	Description string
	TaskCount   int
	Path        string
	Scope       PlanScope
	Args        map[string]any
}

// Project reports whether this plan came from the workspace.
func (plan SavedPlan) Project() bool { return plan.Scope == PlanScopeProject }

// validPlanName is an ALLOW-LIST, and it is the path guard as well as the name
// guard: no separator, no dot, no traversal component can be spelled with these
// characters, so "../../etc/passwd" is refused by the same rule that refuses a
// space. A deny-list of dangerous sequences is the pattern that has leaked
// repeatedly in this repo.
func validPlanName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return planIDPattern.MatchString(name)
}

// SavePlan writes a validated plan under dir as name.json.
//
// It REFUSES to follow a symlink, on the directory and on the file. A saved
// plan is written into a repo-checked-in location, and a `.zero/plans/x.json`
// symlinked at ~/.ssh/config would otherwise make "save my plan" a file
// overwrite primitive.
func SavePlan(root, dir, name string, plan Plan) (string, error) {
	return savePlan(root, dir, name, plan, true)
}

// SavePlanExclusive publishes a validated plan only when name is unused.
// Generated restart/resume staging must never replace a user-authored plan
// that happens to have the same name. The final hard link is an atomic
// no-replace publish: checking first and then calling SavePlan would retain a
// window in which another process could create the destination.
func SavePlanExclusive(root, dir, name string, plan Plan) (string, error) {
	return savePlan(root, dir, name, plan, false)
}

func savePlan(root, dir, name string, plan Plan, replace bool) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("no directory to save plans in")
	}
	if !validPlanName(name) {
		return "", fmt.Errorf("plan name %q must use only letters, digits, hyphen and underscore, and be at most 64 characters", name)
	}
	handle, relative, err := pathjail.Open(root, dir)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	if err := handle.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+planFileExt)
	relativePath := filepath.Join(relative, name+planFileExt)
	if err := pathjail.RefuseReparse(handle, relativePath); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(plan.Args(), "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode plan: %w", err)
	}
	// Write-then-rename, so a crash mid-write leaves the previous plan intact
	// rather than a truncated file that fails to parse on the next run.
	//
	// THE BYTES GO TO THE TEMP FILE, so that is the path that has to be safe —
	// dir and path were both checked and the write went to neither. Creating it
	// with os.CreateTemp rather than checking a fixed <name>.json.tmp buys two
	// things a check could not:
	//
	//   O_EXCL. Lstat-then-write leaves a window: a symlink planted between the
	//   check and the write is followed, and "save my plan" becomes the
	//   file-overwrite primitive the checks above exist to prevent. O_EXCL
	//   refuses to open anything that already exists, symlink or not, so there
	//   is no window to hit.
	//
	//   A unique name. Two saves of the same plan shared one <name>.json.tmp and
	//   stomped each other's bytes; whichever renamed second won with a file the
	//   other had half-written.
	//
	// The random suffix keeps the ".tmp" extension, and ListPlans matches
	// planFileExt exactly, so a temp file left by a crash is never listed as a
	// plan.
	file, temp, err := pathjail.CreateTemp(handle, relative, name, planTempExt)
	if err != nil {
		return "", fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	// Named before any early return: every failure below has to remove it.
	writeErr := func() error {
		if _, err := file.Write(append(body, '\n')); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return file.Close()
	}()
	if writeErr != nil {
		_ = file.Close()
		_ = handle.Remove(temp)
		return "", writeErr
	}
	// CreateTemp makes the file 0600 already; this is belt-and-braces against a
	// umask-sensitive platform, and it runs before the rename so the plan is
	// never briefly world-readable under its real name.
	if !replace {
		if err := handle.Link(temp, relativePath); err != nil {
			_ = handle.Remove(temp)
			if errors.Is(err, fs.ErrExist) {
				return "", fmt.Errorf("saved plan %q already exists; refusing to overwrite it", name)
			}
			return "", fmt.Errorf("save %s without replacing an existing plan: %w", path, err)
		}
		_ = handle.Remove(temp)
		return path, nil
	}
	if err := handle.Rename(temp, relativePath); err != nil {
		_ = handle.Remove(temp)
		return "", fmt.Errorf("save %s: %w", path, err)
	}
	return path, nil
}

// LoadPlans reads every saved plan under the given paths, project shadowing
// user. It returns the plans sorted by name and, separately, the files it could
// not read — malformed data is reported, never silently skipped.
func LoadPlans(paths PlanPaths) (plans []SavedPlan, problems []string) {
	byName := map[string]SavedPlan{}
	// BUILTIN FIRST, so anything on disk shadows it: a bundled plan is an
	// example, never an override of something a user wrote.
	for _, plan := range builtinPlans() {
		byName[plan.Name] = plan
	}
	// Then user, then project, so a project plan of the same name wins. A failed
	// higher-precedence scope blocks lower entries instead of silently running a
	// different plan than the user named.
	for _, source := range []struct {
		root  string
		dir   string
		scope PlanScope
	}{
		{paths.UserRoot, paths.UserDir, PlanScopeUser},
		{paths.ProjectRoot, paths.ProjectDir, PlanScopeProject},
	} {
		dir := source.dir
		if strings.TrimSpace(dir) == "" {
			continue
		}
		root := source.root
		if strings.TrimSpace(root) == "" {
			root = dir
		}
		found, bad, blocked, unavailable := loadPlanDir(root, dir, source.scope)
		problems = append(problems, bad...)
		if unavailable {
			clear(byName)
		}
		for _, name := range blocked {
			delete(byName, name)
		}
		for _, plan := range found {
			byName[plan.Name] = plan
		}
	}
	for _, plan := range byName {
		plans = append(plans, plan)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Name < plans[j].Name })
	sort.Strings(problems)
	return plans, problems
}

func loadPlanDir(root, dir string, scope PlanScope) (plans []SavedPlan, problems []string, blocked []string, unavailable bool) {
	handle, relative, err := pathjail.OpenExisting(root, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, false
		}
		return nil, []string{fmt.Sprintf("%s: %v", dir, err)}, nil, true
	}
	defer handle.Close()
	entries, err := fs.ReadDir(handle.FS(), filepath.ToSlash(relative))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, false
		}
		return nil, []string{fmt.Sprintf("%s: %v", dir, err)}, nil, true
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), planFileExt) {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		path := filepath.Join(dir, entry.Name())
		if !validPlanName(name) {
			problems = append(problems, fmt.Sprintf("%s: name is not a valid plan name", path))
			continue
		}
		relativePath := filepath.Join(relative, entry.Name())
		if err := pathjail.RefuseReparse(handle, relativePath); err != nil {
			problems = append(problems, fmt.Sprintf("%s: symlink or reparse point refused: %v", path, err))
			blocked = append(blocked, name)
			continue
		}
		raw, err := fs.ReadFile(handle.FS(), filepath.ToSlash(relativePath))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			blocked = append(blocked, name)
			continue
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			blocked = append(blocked, name)
			continue
		}
		plans = append(plans, SavedPlan{
			Name:        name,
			Description: planString(args, "description"),
			TaskCount:   savedTaskCount(args),
			Path:        path,
			Scope:       scope,
			Args:        args,
		})
	}
	return plans, problems, blocked, false
}

// savedTaskCount is for LISTING only — a length for a display line, before any
// validation has happened. The authoritative count is Plan.TaskCount, after
// ParsePlan; nothing decides anything from this number.
func savedTaskCount(args map[string]any) int {
	list, _ := args["tasks"].([]any)
	return len(list)
}

// FindSavedPlan returns the named plan, with project shadowing user.
func FindSavedPlan(paths PlanPaths, name string) (SavedPlan, error) {
	if !validPlanName(name) {
		return SavedPlan{}, fmt.Errorf("plan name %q must use only letters, digits, hyphen and underscore", name)
	}
	plans, problems := LoadPlans(paths)
	for _, plan := range plans {
		if plan.Name == name {
			return plan, nil
		}
	}
	if len(problems) > 0 {
		// Say that something was unreadable. "No plan named x" while x sits on
		// disk unparseable is a lie by omission.
		return SavedPlan{}, fmt.Errorf("no saved plan named %q; some plan files could not be read: %s",
			name, strings.Join(problems, "; "))
	}
	return SavedPlan{}, fmt.Errorf("no saved plan named %q", name)
}
