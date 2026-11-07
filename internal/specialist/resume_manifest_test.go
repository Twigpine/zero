package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
)

// narrowManifest is what a plan task carries: an inline definition with a grant
// already intersected down to what the parent run holds.
func narrowManifest() Manifest {
	// EXACTLY what planTaskManifest builds, Metadata.Tools included. Validate
	// re-resolves the tool list from Metadata.Tools, so a fixture that set only
	// ResolvedTools would be re-expanded to the default read-only category —
	// which is what the first version of this test did, and it then "failed"
	// against correct code.
	return planTaskManifest("explorer", "", "", []string{"grep"})
}

func enabledToolsOf(args []string) string {
	for index, arg := range args {
		if arg == "--enabled-tools" && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func resumableSession(t *testing.T) (*sessions.Store, sessions.Metadata) {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{
		SessionID:   "specialist_00000000000000000000000a",
		SessionKind: sessions.SessionKindChild,
		Cwd:         t.TempDir(),
		AgentName:   "explorer",
		Tag:         sessionTagSpecialist,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return store, session
}

// RESUMING A TASK MUST NEVER GRANT IT MORE THAN LAUNCHING IT DID.
//
// runResume looked the manifest up by session.AgentName and ignored
// params.Manifest, so a plan task launched under a parent holding only grep —
// inline grant [grep] — came back from a resume holding the registered
// explorer's five tools. The parent-grant narrowing was undone by resuming.
func TestResumeDoesNotWidenAnInlineGrant(t *testing.T) {
	store, session := resumableSession(t)
	var resumeArgs []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) {
			// The REGISTERED explorer is deliberately wider than the plan's
			// grant — that difference is the defect.
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer", Description: "registered"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"glob", "grep", "list_directory", "read_file", "read_minified_file"},
				ToolsResolved: true,
				Location:      LocationBuiltin,
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			resumeArgs = args
			return ChildRunResult{Started: true}, nil
		},
	}

	manifest := narrowManifest()
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "carry on", Resume: session.SessionID, Manifest: &manifest},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := enabledToolsOf(resumeArgs); got != "grep" {
		t.Fatalf("the resumed child was granted %q, want the inline grant \"grep\": resuming widened its authority", got)
	}
}

// THE SIBLING COMPARISON. Launching and resuming are two doors onto "what may
// this child do?", so the relationship is EQUALITY: the same params must
// produce the same grant through either.
func TestFreshAndResumeResolveTheSameManifest(t *testing.T) {
	store, session := resumableSession(t)
	var fresh, resumed []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000b", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"glob", "grep", "list_directory"},
				ToolsResolved: true,
				Location:      LocationBuiltin,
			}}}, nil
		},
	}

	manifest := narrowManifest()
	executor.RunChild = func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
		fresh = args
		return ChildRunResult{Started: true}, nil
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "go", Manifest: &manifest},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("fresh Run: %v", err)
	}

	executor.RunChild = func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
		resumed = args
		return ChildRunResult{Started: true}, nil
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "go on", Resume: session.SessionID, Manifest: &manifest},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	if enabledToolsOf(fresh) != enabledToolsOf(resumed) {
		t.Fatalf("fresh grants %q and resume grants %q for the same manifest",
			enabledToolsOf(fresh), enabledToolsOf(resumed))
	}
}

// A caller that supplies NO manifest still resolves by name, so an ordinary
// Task resume is unchanged.
func TestResumeWithoutAnInlineManifestStillResolvesByName(t *testing.T) {
	store, session := resumableSession(t)
	var args []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"glob", "grep"},
				ToolsResolved: true,
				Location:      LocationBuiltin,
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, a []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			args = a
			return ChildRunResult{Started: true}, nil
		},
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Prompt: "carry on", Resume: session.SessionID},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := enabledToolsOf(args); got != "glob,grep" {
		t.Fatalf("grant = %q, want the registered manifest's when none was supplied", got)
	}
}

// An inline manifest is VALIDATED on resume exactly as on a fresh launch —
// honouring the caller's definition must not mean trusting it unchecked.
func TestAnInvalidInlineManifestIsRefusedOnResume(t *testing.T) {
	store, session := resumableSession(t)
	executor := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{Started: true}, nil
		},
	}
	broken := Manifest{Metadata: Metadata{Name: ""}}
	_, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "x", Resume: session.SessionID, Manifest: &broken},
		TaskRunOptions{Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "inline specialist manifest") {
		t.Fatalf("an invalid inline manifest must be refused on resume, got %v", err)
	}
}

// The session's identity still wins: resuming another specialist's session with
// a mismatched name is refused, inline manifest or not.
func TestResumeStillRefusesAMismatchedSpecialist(t *testing.T) {
	store, session := resumableSession(t)
	executor := Executor{BinaryPath: "/bin/true", SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil }}
	manifest := narrowManifest()
	_, err := executor.Run(context.Background(),
		TaskParameters{Name: "reviewer", Prompt: "x", Resume: session.SessionID, Manifest: &manifest},
		TaskRunOptions{Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "belongs to specialist") {
		t.Fatalf("a mismatched specialist must still be refused, got %v", err)
	}
}
