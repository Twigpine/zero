package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
)

func modelArgOf(t *testing.T, args []string) string {
	t.Helper()
	for index, arg := range args {
		if arg == "--model" && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func effortArgOf(t *testing.T, args []string) string {
	t.Helper()
	for index, arg := range args {
		if arg == "--reasoning-effort" && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func resumeTestManifest() Manifest {
	return Manifest{
		Metadata:      Metadata{Name: "explorer"},
		ResolvedTools: []string{"read_file"},
		ToolsResolved: true,
	}
}

// THE SIBLING COMPARISON, and the relationship is EQUALITY (RULES.md §3):
// launching a specialist fresh and resuming it are two doors onto the same
// question — which model does this specialist run on? — so they must answer it
// identically.
//
// They did not. BuildResumeArgsInput carried no model fields and
// BuildResumeArgs never called appendModelArgs, so a resumed specialist ran on
// whatever its own config resolved while a fresh one ran on the parent's model.
func TestFreshAndResumedLaunchesAgreeOnTheModel(t *testing.T) {
	executor := Executor{BinaryPath: "/bin/true"}
	manifest := resumeTestManifest()

	fresh, err := executor.BuildArgs(BuildArgsInput{
		Manifest: manifest, Prompt: "x", Cwd: t.TempDir(),
		ParentModel: "parent-chose-this", ParentReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	resumed, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "specialist_00000000000000000000000a", Prompt: "x",
		Manifest: manifest, Cwd: t.TempDir(),
		ParentModel: "parent-chose-this", ParentReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs: %v", err)
	}

	freshModel, resumedModel := modelArgOf(t, fresh.Args), modelArgOf(t, resumed.Args)
	if freshModel != resumedModel {
		t.Fatalf("fresh launches on %q and resume on %q; the same specialist must not change model on resume",
			freshModel, resumedModel)
	}
	if resumedModel != "parent-chose-this" {
		t.Fatalf("resume model = %q, want the parent's", resumedModel)
	}
	if got := effortArgOf(t, resumed.Args); got != effortArgOf(t, fresh.Args) {
		t.Fatalf("reasoning effort differs between fresh (%q) and resume (%q)", effortArgOf(t, fresh.Args), got)
	}
}

// A manifest that pins its own model still wins on resume, exactly as it does
// on a fresh launch — appendModelArgs' rule, applied once rather than twice.
func TestAManifestModelStillWinsOnResume(t *testing.T) {
	executor := Executor{BinaryPath: "/bin/true"}
	manifest := resumeTestManifest()
	manifest.Metadata.Model = "manifest-pinned"

	resumed, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "specialist_00000000000000000000000a", Prompt: "x",
		Manifest: manifest, Cwd: t.TempDir(), ParentModel: "parent-chose-this",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs: %v", err)
	}
	if got := modelArgOf(t, resumed.Args); got != "manifest-pinned" {
		t.Fatalf("model = %q, want the manifest's own pin", got)
	}
}

// With no parent model supplied nothing is forced, so a caller that wires
// neither is unchanged — the resume path stays exactly as permissive as the
// fresh one.
func TestResumeWithoutAParentModelPassesNoFlag(t *testing.T) {
	executor := Executor{BinaryPath: "/bin/true"}
	resumed, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "specialist_00000000000000000000000a", Prompt: "x",
		Manifest: resumeTestManifest(), Cwd: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs: %v", err)
	}
	if strings.Contains(strings.Join(resumed.Args, " "), "--model") {
		t.Fatal("with no parent model the resume path must force nothing")
	}
}

// THE CALL SITE, not the builder. BuildResumeArgs having the fields proves
// nothing if runResume never fills them — which is precisely how the fresh path
// ended up carrying a model the resume path did not. This drives Executor.Run
// with Resume set and reads the argv the child would have been launched with.
func TestResumeCarriesTheRunsModelThroughExecutorRun(t *testing.T) {
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

	var childArgs []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{resumeTestManifest()}}, nil
		},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			childArgs = args
			return ChildRunResult{Started: true}, nil
		},
	}

	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "carry on", Resume: session.SessionID},
		TaskRunOptions{
			Cwd:                   t.TempDir(),
			ParentModel:           "parent-chose-this",
			ParentReasoningEffort: "high",
		}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := modelArgOf(t, childArgs); got != "parent-chose-this" {
		t.Fatalf("the resumed child was launched with model %q, not the run's:\n%s",
			got, strings.Join(childArgs, " "))
	}
}
