package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// nestedGitWorkspace returns an ancestor repository and a workspace inside it
// that carries no git metadata of its own, which is the shape the whole refusal
// is about.
func nestedGitWorkspace(t *testing.T) (ancestor string, workspace string) {
	t.Helper()
	ancestor = t.TempDir()
	if err := os.MkdirAll(filepath.Join(ancestor, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace = filepath.Join(ancestor, "packages", "app")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".git")); err == nil {
		t.Fatal("SETUP INVALID: the workspace carries its own .git, which is the protected case, not this one")
	}
	return ancestor, workspace
}

func gitCommandRequest(workspace string, command string) Request {
	return Request{
		ToolName:       "bash",
		SideEffect:     SideEffectShell,
		Permission:     PermissionPrompt,
		PermissionMode: PermissionModeAsk,
		WorkspaceRoot:  workspace,
		Args:           map[string]any{"command": command},
	}
}

func gitWorkspaceEngine(t *testing.T, workspace string) *Engine {
	t.Helper()
	return NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       nativeWrappingBackend,
	})
}

// THE LIFECYCLE, END TO END: the workspace is protected at write time AND the
// command that would create a repository there is refused with an explanation.
//
// The setup guard is still load-bearing, but it now asserts the opposite of
// what it once did. The carveouts were restored for a nested workspace, because
// skipping them left nothing denying writes to <root>/.git and a repository
// could be assembled there without running git at all. So the refusal is no
// longer the only thing standing between a sandboxed command and a writable
// config; it is the layer that tells an operator why, while the deny ACE is
// what stops it. If the carveouts disappear again this fails here, because the
// refusal on its own does not cover mkdir.
func TestNestedWorkspaceRefusesGitInit(t *testing.T) {
	ancestor, workspace := nestedGitWorkspace(t)

	carved := map[string]bool{}
	for _, carveout := range gitMetadataWriteCarveouts(workspace) {
		carved[filepath.Base(carveout)] = true
	}
	if !carved["config"] || !carved["hooks"] {
		t.Fatalf("SETUP INVALID: the nested workspace planned %v, so config and hooks are not denied and the refusal below is the only protection again", gitMetadataWriteCarveouts(workspace))
	}

	engine := gitWorkspaceEngine(t, workspace)
	decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, "git init"))
	if decision.Action != ActionDeny {
		t.Fatalf("git init inside the repository at %s was %s, not denied; the repository it creates gets a writable config and hooks under the workspace grant", ancestor, decision.Action)
	}
	if decision.Block == nil || decision.Block.Code != BlockNestedGitInit {
		t.Fatalf("block = %#v, want code %s so the operator gets the nested-repository explanation and not a generic denial", decision.Block, BlockNestedGitInit)
	}
}

// The refusal sits ahead of every allow path in Evaluate, so a tool already
// cleared by permissions does not walk through it.
func TestNestedGitInitRefusalOutranksAnAllowPermission(t *testing.T) {
	_, workspace := nestedGitWorkspace(t)
	engine := gitWorkspaceEngine(t, workspace)

	request := gitCommandRequest(workspace, "git init")
	request.Permission = PermissionAllow
	if decision := engine.Evaluate(context.Background(), request); decision.Action != ActionDeny {
		t.Fatalf("an allow permission carried git init through: %#v", decision)
	}

	request = gitCommandRequest(workspace, "git init")
	request.PermissionMode = PermissionUnsafe
	request.PermissionGranted = true
	if decision := engine.Evaluate(context.Background(), request); decision.Action != ActionDeny {
		t.Fatalf("unsafe mode carried git init through: %#v", decision)
	}
}

// The global options that bypassed the network gate must not bypass this one.
// These are the exact spellings that classified a clone as network=false before
// gitSubcommand learned which options take a value.
func TestNestedGitInitRefusalSurvivesGitGlobalOptions(t *testing.T) {
	_, workspace := nestedGitWorkspace(t)
	engine := gitWorkspaceEngine(t, workspace)

	for _, command := range []string{
		"git init",
		"git init-db",
		"git -C packages/app init",
		"git -c core.hooksPath=/tmp/h init",
		"git --git-dir=/tmp/g init",
		"git --namespace ns init",
		"sh -c \"git init\"",
		"sudo git init",
		"git status && git init",
	} {
		t.Run(command, func(t *testing.T) {
			decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, command))
			if decision.Action != ActionDeny {
				t.Fatalf("%q was %s, not denied", command, decision.Action)
			}
		})
	}
}

// CONTROL: ordinary git in a nested workspace is untouched.
//
// Discovery still walks up to the ancestor and every command that uses it keeps
// working; only creation is refused. Without this, returning deny for anything
// containing the word git would pass the tests above.
func TestNestedWorkspaceStillRunsOrdinaryGitCommands(t *testing.T) {
	_, workspace := nestedGitWorkspace(t)
	engine := gitWorkspaceEngine(t, workspace)

	for _, command := range []string{
		"git status",
		"git add .",
		"git commit -m init",
		"git log --oneline",
		"git worktree list",
		"git submodule update --init",
	} {
		t.Run(command, func(t *testing.T) {
			decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, command))
			if decision.Action == ActionDeny {
				t.Fatalf("%q was denied in a nested workspace: %s", command, decision.Reason)
			}
		})
	}
}

// CONTROL: a standalone workspace still creates repositories.
//
// It keeps its materialized config and hooks carveouts, so the protection the
// refusal stands in for is actually there and the command is allowed.
func TestStandaloneWorkspaceStillAllowsGitInit(t *testing.T) {
	workspace := t.TempDir()
	if gitMetadataGovernedByAncestor(workspace) {
		t.Skip("this temp directory sits inside a repository, so it is not the standalone case")
	}
	if len(gitMetadataWriteCarveouts(workspace)) == 0 {
		t.Fatal("SETUP INVALID: a standalone workspace planned no carveouts, so it has no protection to allow git init against")
	}

	engine := gitWorkspaceEngine(t, workspace)
	decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, "git init"))
	if decision.Action == ActionDeny {
		t.Fatalf("git init was refused in a standalone workspace: %s", decision.Reason)
	}
}

// CONTROL: a linked worktree is a workspace whose .git is a FILE.
//
// It owns git metadata, keeps its pointer-file carveout, and is not somebody
// else's repository, so nothing here is refused. Zero's own development
// worktrees are this shape, so getting it wrong would refuse git init for the
// people writing this code.
func TestLinkedWorktreeWorkspaceStillAllowsGitInit(t *testing.T) {
	ancestor := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ancestor, ".git", "worktrees", "w"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(ancestor, "checkout")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(workspace, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: ../.git/worktrees/w\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// SETUP: the pointer carveout is intact, or this proves nothing about
	// worktrees keeping their protection.
	specs := gitMetadataWriteCarveoutSpecs(workspace)
	if len(specs) != 1 || specs[0].Path != pointer || !specs[0].IsFile {
		t.Fatalf("SETUP INVALID: linked worktree carveouts = %+v, want the file-shaped pointer %s", specs, pointer)
	}

	engine := gitWorkspaceEngine(t, workspace)
	if decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, "git init")); decision.Action == ActionDeny {
		t.Fatalf("git init was refused in a linked worktree, which owns its git metadata: %s", decision.Reason)
	}
}

// And the workspace-level question is asked of the workspace itself, so a
// workspace that already carries .git is never treated as nested no matter what
// is above it.
func TestWorkspaceOwningGitIsNotTreatedAsNested(t *testing.T) {
	ancestor := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ancestor, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(ancestor, "vendored")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if workspaceGovernedByAncestorRepository(workspace) {
		t.Fatal("a workspace that owns .git was reported as governed by its ancestor, so its own protection would be refused instead of applied")
	}
}

// CLONE IS CREATION TOO, AND THE NETWORK GRANT DOES NOT ANSWER FOR IT.
//
// "git clone <url> ." into a workspace governed by an ancestor repository lands
// a root .git whose config and hooks setup never planned carveouts for, exactly
// the state git init produces, but the guard only knew the init spelling. With
// network explicitly allowed and the shell grant already given, the clone came
// back as an ordinary allow. The repository-creation refusal is a separate
// decision from the network prompt and has to stand on its own. Reported by
// @gnanam1990.
func TestNestedWorkspaceRefusesGitCloneIntoTheRoot(t *testing.T) {
	ancestor, workspace := nestedGitWorkspace(t)
	policy := DefaultPolicy()
	policy.Network = NetworkAllow
	engine := NewEngine(EngineOptions{WorkspaceRoot: workspace, Policy: policy, Backend: nativeWrappingBackend})
	for _, command := range []string{
		"git clone https://example.invalid/repo.git .",
		"git -C . clone https://example.invalid/repo.git .",
		"git clone --depth 1 https://example.invalid/repo.git vendor/dep",
	} {
		request := gitCommandRequest(workspace, command)
		request.Permission = PermissionAllow
		request.PermissionGranted = true
		decision := engine.Evaluate(context.Background(), request)
		if decision.Action != ActionDeny {
			t.Fatalf("%q inside the repository at %s was %s, not denied; the repository it creates gets a writable config and hooks under the workspace grant", command, ancestor, decision.Action)
		}
		if decision.Block == nil || decision.Block.Code != BlockNestedGitInit {
			t.Fatalf("%q block = %#v, want code %s", command, decision.Block, BlockNestedGitInit)
		}
	}
	// CONTROLS: ordinary git operations and the submodule init form stay clear
	// of this guard, and a standalone workspace is not governed by anything.
	for _, command := range []string{"git status", "git submodule update --init", "git fetch origin"} {
		request := gitCommandRequest(workspace, command)
		request.Permission = PermissionAllow
		request.PermissionGranted = true
		if decision := engine.Evaluate(context.Background(), request); decision.Action == ActionDeny && decision.Block != nil && decision.Block.Code == BlockNestedGitInit {
			t.Fatalf("%q was refused as nested repository creation; it creates no repository", command)
		}
	}
	standalone := t.TempDir()
	if workspaceGovernedByAncestorRepository(standalone) {
		t.Fatalf("SETUP INVALID: %s is governed by an ancestor repository", standalone)
	}
	request := gitCommandRequest(standalone, "git clone https://example.invalid/repo.git .")
	request.Permission = PermissionAllow
	request.PermissionGranted = true
	if decision := gitWorkspaceEngine(t, standalone).Evaluate(context.Background(), request); decision.Block != nil && decision.Block.Code == BlockNestedGitInit {
		t.Fatalf("clone into a standalone workspace was refused as nested: %#v", decision)
	}
}

// AN INLINE ALIAS IS STILL INIT. The refusal used to read the literal
// subcommand token, and `git -c alias.bootstrap=init bootstrap` passed it with
// shell and network granted and created a repository the profile carves nothing
// out for. The alias is resolved to the subcommand git runs; a chained alias
// follows the chain; a shell alias, which this analyzer cannot classify, counts
// as creation under this guard and nowhere else.
func TestNestedWorkspaceRefusesAnInlineGitAliasForInit(t *testing.T) {
	ancestor, workspace := nestedGitWorkspace(t)
	policy := DefaultPolicy()
	policy.Network = NetworkAllow
	engine := NewEngine(EngineOptions{WorkspaceRoot: workspace, Policy: policy, Backend: nativeWrappingBackend})
	for _, command := range []string{
		"git init",
		"git -c alias.bootstrap=init bootstrap",
		"git -c alias.mk=init-db mk",
		"git -c alias.get=clone get https://example.invalid/repo.git .",
		"git -c alias.two=bootstrap -c alias.bootstrap=init two",
		"git -c alias.a=b -c alias.b=c -c alias.c=d -c alias.d=e -c alias.e=init a .",
		"git -c alias.a=b -c alias.b=a a",
		"git -c alias.dangling=nowhere -c alias.nowhere= dangling",
		"git -C . -c alias.bootstrap=init bootstrap",
		"git -c alias.sh=!sh -c 'git init' sh",
		// THE EXPANSION IS PARSED THE WAY GIT PARSES IT. A -c inside the
		// expansion defines the alias the next lookup uses, and quotes are
		// stripped before the subcommand is read; both of the first two
		// spellings created a repository while only the first whitespace
		// field of the expansion was classified.
		"git -c alias.a='-c alias.b=init b' a .",
		"git -c 'alias.a=\"init\"' a .",
		"git -c 'alias.a=in\"it\"' a",
		"git -c 'alias.a=in\\it' a",
		"git -c alias.a='--no-pager init' a",
		"git -c alias.a='-c alias.a=init a' a",
		// Git dies on these before running anything; refusing them costs
		// nothing and keeps the rule that an expansion this analyzer cannot
		// read is not evidence of safety.
		"git -c 'alias.a=\"in' a",
		"git -c 'alias.a=-C . init' a",
		// An alias defined by reference, or through the environment the call
		// sets, is the same command with its definition moved out of sight.
		"git --config-env=alias.a=zz a .",
		"git --config-env alias.a=zz a .",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.a GIT_CONFIG_VALUE_0=init git a .",
		"env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.a GIT_CONFIG_VALUE_0=init git a .",
		"GIT_CONFIG_PARAMETERS=\"'alias.a=init'\" git a .",
		"GIT_CONFIG_PARAMETERS=$params git a .",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.a GIT_CONFIG_VALUE_0=$cmd git a .",
	} {
		request := gitCommandRequest(workspace, command)
		request.Permission = PermissionAllow
		request.PermissionGranted = true
		decision := engine.Evaluate(context.Background(), request)
		if decision.Action != ActionDeny || decision.Block == nil || decision.Block.Code != BlockNestedGitInit {
			t.Fatalf("%q inside the repository at %s was not refused as nested repository creation: action=%s block=%#v", command, ancestor, decision.Action, decision.Block)
		}
	}
	// CONTROLS: a harmless alias, an unrelated -c setting, ordinary use, and
	// the quoted, option-bearing and environment-defined spellings of an alias
	// that runs something harmless.
	for _, command := range []string{
		"git -c alias.st=status st",
		"git -c core.autocrlf=false status",
		"git -c alias.bootstrap=init status",
		"git -c alias.lg=log lg --oneline",
		"git -c alias.a=b -c alias.b=c -c alias.c=d -c alias.d=e -c alias.e=status a",
		"git -c alias.st='-c color.ui=always status' st",
		"git -c 'alias.st=\"status\"' st",
		"git -c 'alias.st=sta\"tus\"' st",
		"git -c alias.l='--no-pager log --oneline' l",
		"git -c alias.s=st -c 'alias.st=\"status\"' s",
		"git --config-env=core.pager=p status",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.st GIT_CONFIG_VALUE_0=status git st",
		"GIT_CONFIG_PARAMETERS=\"'alias.st=status'\" git st",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.pager GIT_CONFIG_VALUE_0=$pager git log",
	} {
		request := gitCommandRequest(workspace, command)
		request.Permission = PermissionAllow
		request.PermissionGranted = true
		if decision := engine.Evaluate(context.Background(), request); decision.Action == ActionDeny && decision.Block != nil && decision.Block.Code == BlockNestedGitInit {
			t.Fatalf("%q was refused as nested repository creation; it creates no repository", command)
		}
	}
	// And a standalone workspace is not governed at all, alias or not.
	standalone := t.TempDir()
	for _, command := range []string{
		"git -c alias.bootstrap=init bootstrap",
		"git -c alias.a='-c alias.b=init b' a .",
		"git -c 'alias.a=\"init\"' a .",
		"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.a GIT_CONFIG_VALUE_0=init git a .",
	} {
		request := gitCommandRequest(standalone, command)
		request.Permission = PermissionAllow
		request.PermissionGranted = true
		if decision := gitWorkspaceEngine(t, standalone).Evaluate(context.Background(), request); decision.Block != nil && decision.Block.Code == BlockNestedGitInit {
			t.Fatalf("%q in a standalone workspace was refused as nested: %#v", command, decision)
		}
	}
}

// THE REFUSAL THAT CANNOT BE APPROVED IS THE ONE THE OPERATOR HAS TO SEE.
//
// `git clone` carries both risks: it uses the network and it creates a
// repository. The clone test above sets NetworkAllow, which is the only policy
// under which the nested-repository branch was ever reached. Under the DEFAULT
// policy the network gate ran first and answered with a prompt, so the operator
// was asked to approve network access for a command no approval could make
// runnable, and ReasonNestedGitInit, the reason with a remedy in it, was never
// shown. Reported by @jatmn.
func TestNestedCloneIsRefusedAsNestedUnderTheDefaultNetworkPolicy(t *testing.T) {
	_, workspace := nestedGitWorkspace(t)
	policy := DefaultPolicy()
	if policy.Network != NetworkDeny {
		t.Fatalf("SETUP INVALID: the default network policy is %q, so this does not exercise the ordering", policy.Network)
	}
	engine := NewEngine(EngineOptions{WorkspaceRoot: workspace, Policy: policy, Backend: nativeWrappingBackend})

	for _, granted := range []bool{false, true} {
		request := gitCommandRequest(workspace, "git clone https://example.invalid/repo.git vendor/dep")
		request.PermissionGranted = granted
		decision := engine.Evaluate(context.Background(), request)
		if decision.Action != ActionDeny || decision.Block == nil || decision.Block.Code != BlockNestedGitInit {
			t.Fatalf("granted=%v: clone in a governed workspace was %s (block %#v, reason %q), want a %s denial", granted, decision.Action, decision.Block, decision.Reason, BlockNestedGitInit)
		}
		if decision.Reason != ReasonNestedGitInit {
			t.Errorf("granted=%v: reason = %q, want the nested-repository reason and its remedy", granted, decision.Reason)
		}
	}

	// CONTROLS. A network command that creates no repository still gets the
	// network answer, so the reordering did not swallow that gate, and a clone
	// into a standalone workspace is a network question and nothing else.
	fetch := engine.Evaluate(context.Background(), gitCommandRequest(workspace, "git fetch origin"))
	if fetch.Action != ActionPrompt || fetch.Reason != ReasonNetworkBlocked {
		t.Errorf("git fetch in the governed workspace was %s (%q), want the network prompt", fetch.Action, fetch.Reason)
	}
	standalone := t.TempDir()
	if workspaceGovernedByAncestorRepository(standalone) {
		t.Fatalf("SETUP INVALID: %s is governed by an ancestor repository", standalone)
	}
	standaloneEngine := NewEngine(EngineOptions{WorkspaceRoot: standalone, Policy: policy, Backend: nativeWrappingBackend})
	clone := standaloneEngine.Evaluate(context.Background(), gitCommandRequest(standalone, "git clone https://example.invalid/repo.git ."))
	if clone.Action != ActionPrompt || clone.Reason != ReasonNetworkBlocked {
		t.Errorf("clone into a standalone workspace was %s (%q), want the network prompt", clone.Action, clone.Reason)
	}
}

// THE LIMIT OF THIS GUARD, WRITTEN DOWN SO IT IS NOT MISTAKEN FOR COVERAGE.
//
// The nested-repository rule recognises git creating a repository. It does not
// recognise a repository assembled by other means, and it cannot: mkdir and a
// few writes, an archive, or a script in any language all produce the same
// directory, and none of them is a git command. These legs are allowed today.
// They are here so that anyone reading the rule as containment of the .git
// pathname finds a test saying otherwise, and so that a change which DOES close
// it at the decision layer has to come and update this deliberately.
//
// It is not a statement that the behaviour is acceptable. A repository built
// this way carries whatever config its author wrote, and a plain `git status`
// outside the sandbox honours it. That has to be closed where the carveouts
// live, at write time. Reported by @jatmn.
func TestHandAssembledRepositoryIsNotWhatThisGuardCatches(t *testing.T) {
	_, workspace := nestedGitWorkspace(t)
	engine := gitWorkspaceEngine(t, workspace)
	for _, command := range []string{
		"mkdir .git",
		"mkdir -p .git/objects .git/refs && printf 'ref: refs/heads/main\n' > .git/HEAD",
		"cp -r ../template-repo/.git .git",
		"python3 -c \"import os; os.makedirs('.git/refs')\"",
		"tar -xf repo.tar",
	} {
		decision := engine.Evaluate(context.Background(), gitCommandRequest(workspace, command))
		if decision.Block != nil && decision.Block.Code == BlockNestedGitInit {
			t.Errorf("%q is now refused as nested repository creation. If that is deliberate, this test and the WHAT THIS IS NOT note in risk.go both need to say what the rule covers now", command)
		}
	}
}
