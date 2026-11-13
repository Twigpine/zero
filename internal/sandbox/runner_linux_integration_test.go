//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/execution"
)

func TestLinuxHelperRealSandboxSmoke(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real sandbox smoke tests")
	}
	backend := SelectBackend(BackendOptions{})
	if !backend.Available || backend.Name != BackendLinuxBwrap {
		t.Skipf("Linux sandbox backend unavailable: %s", backend.Message)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("Mkdir .git/hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git/config: %v", err)
	}
	credentialHome := t.TempDir()
	configHome := filepath.Join(credentialHome, "config")
	for _, path := range []string{
		filepath.Join(credentialHome, ".aws"),
		filepath.Join(credentialHome, ".config", "gcloud"),
		filepath.Join(credentialHome, ".azure"),
		filepath.Join(configHome, "zero"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("Mkdir credential path: %v", err)
		}
	}
	// Secrets inside those stores, so the deny-read table below probes a real read
	// rather than only the directory's presence.
	awsCredentials := filepath.Join(credentialHome, ".aws", "credentials")
	if err := os.WriteFile(awsCredentials, []byte("[default]\naws_secret_access_key = leaked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile aws credentials: %v", err)
	}
	zeroTokens := filepath.Join(configHome, "zero", "oauth-tokens.json")
	if err := os.WriteFile(zeroTokens, []byte(`{"access_token":"leaked"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile zero tokens: %v", err)
	}
	t.Setenv("HOME", credentialHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("hidden\n"), 0o644); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	blockedDir := filepath.Join(root, "blocked")
	if err := os.Mkdir(blockedDir, 0o755); err != nil {
		t.Fatalf("Mkdir blocked: %v", err)
	}

	policy := DefaultPolicy()
	policy.DenyRead = []string{secretDir}
	policy.DenyWrite = []string{blockedDir}
	engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: policy, Backend: backend})
	output, runErr := runLinuxSandboxSmokeCommand(t, engine, CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", strings.Join([]string{
			"set -eu",
			"test \"$HOME\" != \"$PWD\"",
			"test ! -e .zero && test ! -e .agents",
			"echo cache > \"$npm_config_cache/zero-runtime-probe\"",
			"test ! -e .npm && test ! -e .cache",
			"rm -f \"$npm_config_cache/zero-runtime-probe\"",
			"echo ok > write-ok.txt",
			"test \"$(cat write-ok.txt)\" = ok",
			"echo tmp > /tmp/zero-sandbox-smoke",
			"test \"$(cat /tmp/zero-sandbox-smoke)\" = tmp",
			"cat .git/config >/dev/null",
		}, "\n")},
		Dir: root,
	})
	if runErr != nil {
		if linuxSandboxLaunchUnsupported(string(output)) {
			t.Skipf("Linux sandbox launch is unsupported in this environment: %v\n%s", runErr, output)
		}
		t.Fatalf("allowed smoke command failed: %v\n%s", runErr, output)
	}

	t.Run("fresh home and non-git workspace launch", func(t *testing.T) {
		freshRoot := t.TempDir()
		freshHome := t.TempDir()
		freshEngine := NewEngine(EngineOptions{WorkspaceRoot: freshRoot, Policy: DefaultPolicy(), Backend: backend})
		output, runErr := runLinuxSandboxSmokeCommand(t, freshEngine, CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", "echo ok > launched"},
			Dir:  freshRoot,
			Env:  []string{"HOME=" + freshHome, "XDG_CONFIG_HOME=" + filepath.Join(freshHome, ".config")},
		})
		if runErr != nil {
			t.Fatalf("fresh environment failed to launch: %v\n%s", runErr, output)
		}
	})

	t.Run("missing command credential root fails closed without host mutation", func(t *testing.T) {
		commandRoot := filepath.Join(tempDirOutsideDefaultTemp(t), "missing-command-home")
		commandConfig := filepath.Join(commandRoot, "config")
		launched := filepath.Join(root, "command-credential-root-launched")
		engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})
		_, err := engine.BuildCommandPlan(CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", "echo launched > " + shellQuote(launched)},
			Dir:  root,
			Env:  []string{"HOME=" + commandRoot, "XDG_CONFIG_HOME=" + commandConfig},
		})
		if err == nil || !strings.Contains(err.Error(), "created after launch") {
			t.Fatalf("BuildCommandPlan error = %v, want future credential-directory failure", err)
		}
		for _, path := range []string{commandRoot, commandConfig, launched} {
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("failed-closed planning materialized command-controlled path %q: %v", path, statErr)
			}
		}
	})

	// The mid-session race the EnsureDenyReadDirs contract exists for: nothing
	// under $XDG_CONFIG_HOME exists when the plan is built, so bubblewrap would
	// have had no mount destination to mask. The sandbox creates Zero's own
	// directory first, and the token written by this test WHILE the sandboxed
	// command is already running must stay invisible to it.
	t.Run("credential store created during the session stays hidden", func(t *testing.T) {
		raceHome := t.TempDir()
		raceConfig := filepath.Join(raceHome, "config")
		raceStore := filepath.Join(raceConfig, "zero")
		tokenPath := filepath.Join(raceStore, "oauth-tokens.json")
		raceRoot := t.TempDir()
		started := filepath.Join(raceRoot, "started")
		ready := filepath.Join(raceRoot, "ready")
		done := make(chan struct{})
		go func() {
			defer close(done)
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(started); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The namespace is assembled by now, so this write is the concurrent
			// login the sandbox must not be able to read.
			if err := os.MkdirAll(raceStore, 0o700); err == nil {
				_ = os.WriteFile(tokenPath, []byte("racyleak\n"), 0o600)
			}
			_ = os.WriteFile(ready, []byte("1\n"), 0o600)
		}()
		raceEngine := NewEngine(EngineOptions{WorkspaceRoot: raceRoot, Policy: DefaultPolicy(), Backend: backend})
		output, _ := runLinuxSandboxSmokeCommand(t, raceEngine, CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", strings.Join([]string{
				"echo 1 > " + shellQuote(started),
				"i=0",
				"while [ ! -e " + shellQuote(ready) + " ] && [ \"$i\" -lt 2000 ]; do i=$((i+1)); sleep 0.01; done",
				"if cat " + shellQuote(tokenPath) + " 2>/dev/null | grep -q racyleak; then echo MIDSESSION_CREDENTIAL_READ_SUCCEEDED; fi",
			}, "\n")},
			Dir: raceRoot,
			Env: []string{"HOME=" + raceHome, "XDG_CONFIG_HOME=" + raceConfig},
		})
		<-done
		if strings.Contains(string(output), "MIDSESSION_CREDENTIAL_READ_SUCCEEDED") {
			t.Fatalf("token created during the session was readable: %s", output)
		}
		if _, err := os.Stat(tokenPath); err != nil {
			t.Fatalf("test did not create the token it probes for: %v", err)
		}
	})

	// The user plugin root shares the denied credential directory, and its
	// commands are executed through this sandbox, so it must stay readable.
	t.Run("user plugin root inside the denied config dir stays readable", func(t *testing.T) {
		pluginHome := t.TempDir()
		pluginConfig := filepath.Join(pluginHome, "config")
		pluginRoot := filepath.Join(pluginConfig, "zero", "plugins", "demo")
		if err := os.MkdirAll(pluginRoot, 0o700); err != nil {
			t.Fatalf("MkdirAll plugin root: %v", err)
		}
		manifest := filepath.Join(pluginRoot, "plugin.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"demo"}`+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile plugin manifest: %v", err)
		}
		secret := filepath.Join(pluginConfig, "zero", "oauth-tokens.json")
		if err := os.WriteFile(secret, []byte(`{"access_token":"leaked"}`+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile zero tokens: %v", err)
		}
		pluginEngine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})
		output, runErr := runLinuxSandboxSmokeCommand(t, pluginEngine, CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", strings.Join([]string{
				"set -eu",
				"grep -q demo " + shellQuote(manifest),
				"if cat " + shellQuote(secret) + " 2>/dev/null | grep -q leaked; then echo CARVEOUT_LEAKED_SIBLING; exit 42; fi",
			}, "\n")},
			Dir: root,
			Env: []string{"HOME=" + pluginHome, "XDG_CONFIG_HOME=" + pluginConfig},
		})
		if runErr != nil {
			t.Fatalf("plugin root inside the denied config dir was not readable: %v\n%s", runErr, output)
		}
		if strings.Contains(string(output), "CARVEOUT_LEAKED_SIBLING") {
			t.Fatalf("carveout exposed a credential sibling: %s", output)
		}
	})

	t.Run("credential store created after launch stays hidden on the next run", func(t *testing.T) {
		lateHome := t.TempDir()
		lateConfig := filepath.Join(lateHome, "config")
		lateStore := filepath.Join(lateConfig, "zero")
		if err := os.MkdirAll(lateStore, 0o700); err != nil {
			t.Fatalf("MkdirAll late credential store: %v", err)
		}
		if err := os.WriteFile(filepath.Join(lateStore, "oauth-tokens.json"), []byte("token\n"), 0o600); err != nil {
			t.Fatalf("WriteFile late token: %v", err)
		}
		lateEngine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})
		output, runErr := runLinuxSandboxSmokeCommand(t, lateEngine, CommandSpec{
			Name: "/bin/sh",
			Args: []string{"-c", "cat " + shellQuote(filepath.Join(lateStore, "oauth-tokens.json"))},
			Dir:  root,
			Env:  []string{"HOME=" + lateHome, "XDG_CONFIG_HOME=" + lateConfig},
		})
		if runErr == nil || strings.Contains(string(output), "token") {
			t.Fatalf("credential store created before this run stayed readable: err=%v output=%s", runErr, output)
		}
	})

	for _, tc := range []struct {
		name   string
		script string
		marker string
	}{
		{
			name:   "outside write",
			script: "if echo leak > /etc/zero_sandbox_smoke 2>/dev/null; then echo OUTSIDE_WRITE_SUCCEEDED; exit 42; fi",
			marker: "OUTSIDE_WRITE_SUCCEEDED",
		},
		{
			name:   "deny read",
			script: "if cat " + shellQuote(filepath.Join(secretDir, "secret.txt")) + " >/dev/null 2>&1; then echo DENY_READ_SUCCEEDED; exit 42; fi",
			marker: "DENY_READ_SUCCEEDED",
		},
		{
			name:   "deny write",
			script: "if echo leak > blocked/file 2>/dev/null; then echo DENY_WRITE_SUCCEEDED; exit 42; fi",
			marker: "DENY_WRITE_SUCCEEDED",
		},
		{
			name:   "metadata write",
			script: "if echo leak > .git/config 2>/dev/null; then echo METADATA_WRITE_SUCCEEDED; exit 42; fi",
			marker: "METADATA_WRITE_SUCCEEDED",
		},
		{
			name:   "cloud credential store read",
			script: "if cat " + shellQuote(awsCredentials) + " 2>/dev/null | grep -q leaked; then echo CLOUD_CREDENTIAL_READ_SUCCEEDED; exit 42; fi",
			marker: "CLOUD_CREDENTIAL_READ_SUCCEEDED",
		},
		{
			name:   "zero credential store read",
			script: "if cat " + shellQuote(zeroTokens) + " 2>/dev/null | grep -q leaked; then echo ZERO_CREDENTIAL_READ_SUCCEEDED; exit 42; fi",
			marker: "ZERO_CREDENTIAL_READ_SUCCEEDED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, _ := runLinuxSandboxSmokeCommand(t, engine, CommandSpec{
				Name: "/bin/sh",
				Args: []string{"-c", tc.script},
				Dir:  root,
			})
			if strings.Contains(string(output), tc.marker) {
				t.Fatalf("sandbox allowed %s; output=%s", tc.name, output)
			}
		})
	}

	if python, err := exec.LookPath("python3"); err == nil && python != "" {
		t.Run("network deny", func(t *testing.T) {
			output, runErr := runLinuxSandboxSmokeCommand(t, engine, CommandSpec{
				Name: python,
				Args: []string{"-c", "import socket; socket.create_connection(('1.1.1.1', 80), 2).close()"},
				Dir:  root,
			})
			if runErr == nil {
				t.Fatalf("sandbox allowed outbound network; output=%s", output)
			}
		})
	} else {
		t.Log("python3 not found; skipping real network deny probe")
	}
}

func TestLinuxLandlockRealSandboxSmoke(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real sandbox smoke tests")
	}
	helper, err := linuxSandboxHelperCommand()
	if err != nil {
		t.Skipf("Linux sandbox helper unavailable: %v", err)
	}
	root := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "blocked.txt")
	profile := PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                 FileSystemRestricted,
			ReadRoots:            []string{string(filepath.Separator)},
			WriteRoots:           []WritableRoot{{Root: root}},
			IncludePlatformRoots: true,
			AllowTemp:            true,
		},
		Network: NetworkPolicy{Mode: NetworkDeny},
	}
	output, runErr := runLinuxLandlockSmokeCommand(t, helper, profile, root, []string{"/bin/sh", "-c", strings.Join([]string{
		"set -eu",
		"echo ok > " + shellQuote(filepath.Join(root, "write-ok.txt")),
		"test \"$(cat " + shellQuote(filepath.Join(root, "write-ok.txt")) + ")\" = ok",
		"if echo leak > " + shellQuote(outsideFile) + " 2>/dev/null; then echo LANDLOCK_OUTSIDE_WRITE_SUCCEEDED; exit 42; fi",
	}, "\n")})
	if runErr != nil {
		if landlockLaunchUnsupported(string(output)) {
			t.Skipf("Landlock is unsupported in this environment: %v\n%s", runErr, output)
		}
		t.Fatalf("Landlock smoke command failed: %v\n%s", runErr, output)
	}
	if strings.Contains(string(output), "LANDLOCK_OUTSIDE_WRITE_SUCCEEDED") {
		t.Fatalf("Landlock allowed write outside approved roots: %s", output)
	}
	if _, err := os.Stat(outsideFile); err == nil {
		t.Fatalf("Landlock wrote host file outside approved roots: %s", outsideFile)
	}

	if python, err := exec.LookPath("python3"); err == nil && python != "" {
		output, runErr = runLinuxLandlockSmokeCommand(t, helper, profile, root, []string{
			python,
			"-c",
			"import socket; socket.create_connection(('1.1.1.1', 80), 2).close()",
		})
		if runErr == nil {
			t.Fatalf("Landlock mode allowed outbound network; output=%s", output)
		}
	} else {
		t.Log("python3 not found; skipping Landlock network deny probe")
	}
}

func TestLinuxHelperAllowsIsolatedLoopbackWithoutExternalEgress(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real sandbox smoke tests")
	}
	backend := SelectBackend(BackendOptions{})
	if !backend.Available || backend.Name != BackendLinuxBwrap {
		t.Skipf("Linux sandbox backend unavailable: %s", backend.Message)
	}
	python, err := exec.LookPath("python3")
	if err != nil || python == "" {
		t.Skip("python3 not found; skipping isolated loopback probe")
	}

	root := t.TempDir()
	engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})
	loopbackScript := strings.Join([]string{
		"import socket",
		"server = socket.socket()",
		"server.bind(('127.0.0.1', 0))",
		"server.listen()",
		"client = socket.socket()",
		"client.connect(('127.0.0.1', server.getsockname()[1]))",
		"accepted, _ = server.accept()",
		"client.send(b'ok')",
		"assert accepted.recv(2) == b'ok'",
	}, "; ")
	output, runErr := runLinuxSandboxSmokeCommand(t, engine, CommandSpec{
		Name: python,
		Args: []string{"-c", loopbackScript},
		Dir:  root,
	})
	if runErr != nil {
		t.Fatalf("isolated loopback failed: %v\n%s", runErr, output)
	}

	output, runErr = runLinuxSandboxSmokeCommand(t, engine, CommandSpec{
		Name: python,
		Args: []string{"-c", "import socket; socket.create_connection(('1.1.1.1', 80), 1).close()"},
		Dir:  root,
	})
	if runErr == nil {
		t.Fatalf("sandbox allowed external egress while isolated loopback was enabled: %s", output)
	}
}

func TestLinuxHelperPreservesAbsentProtectedMetadata(t *testing.T) {
	if os.Getenv("ZERO_SANDBOX_REAL_SMOKE") != "1" {
		t.Skip("set ZERO_SANDBOX_REAL_SMOKE=1 to run real sandbox smoke tests")
	}
	backend := SelectBackend(BackendOptions{})
	if !backend.Available || backend.Name != BackendLinuxBwrap {
		t.Skipf("Linux sandbox backend unavailable: %s", backend.Message)
	}

	root := t.TempDir()
	engine := NewEngine(EngineOptions{WorkspaceRoot: root, Policy: DefaultPolicy(), Backend: backend})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command, plan, err := engine.CommandContext(ctx, CommandSpec{
		Name: "/bin/sh",
		Args: []string{"-c", "set -eu; test ! -e .zero; mkdir .zero"},
		Dir:  root,
	})
	if err != nil {
		t.Fatalf("CommandContext: %v", err)
	}
	defer plan.Cleanup()
	output, runErr := command.CombinedOutput()
	if runErr == nil {
		t.Fatalf("sandbox reported success after protected metadata creation; output=%s", output)
	}
	if !strings.Contains(string(output), "blocked creation of protected workspace metadata path") {
		t.Fatalf("sandbox did not explain protected metadata denial: %v\n%s", runErr, output)
	}
	if _, err := os.Lstat(filepath.Join(root, ".zero")); !os.IsNotExist(err) {
		t.Fatalf("protected metadata path remained after execution: %v", err)
	}
	report, err := plan.ExecutionReport()
	if err != nil {
		t.Fatalf("ExecutionReport: %v", err)
	}
	if report.Denial == nil || report.Denial.Capability.Kind != execution.CapabilityProtectedMetadata {
		t.Fatalf("execution report denial = %#v, want protected metadata", report.Denial)
	}
	if report.Denial.Capability.Scope != filepath.Join(root, ".zero") {
		t.Fatalf("denial scope = %q, want exact protected path", report.Denial.Capability.Scope)
	}
}

func runLinuxSandboxSmokeCommand(t *testing.T, engine *Engine, spec CommandSpec) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command, plan, err := engine.CommandContext(ctx, spec)
	if err != nil {
		t.Fatalf("CommandContext: %v", err)
	}
	output, runErr := command.CombinedOutput()
	if strings.Contains(string(output), "OUTSIDE_WRITE_SUCCEEDED") {
		t.Fatalf("sandbox allowed write outside workspace; plan=%#v output=%s", plan, output)
	}
	return output, runErr
}

func runLinuxLandlockSmokeCommand(t *testing.T, helper LinuxSandboxHelperCommand, profile PermissionProfile, root string, command []string) ([]byte, error) {
	t.Helper()
	args, err := BuildLinuxSandboxCommandArgs(LinuxSandboxCommandArgsOptions{
		SandboxPolicyCWD:  root,
		CommandCWD:        root,
		PermissionProfile: profile,
		UseLandlock:       true,
		Command:           command,
	})
	if err != nil {
		t.Fatalf("BuildLinuxSandboxCommandArgs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper.Name, append(append([]string{}, helper.ArgsPrefix...), args...)...)
	if helper.Dir != "" {
		cmd.Dir = helper.Dir
	} else {
		cmd.Dir = root
	}
	cmd.Env = os.Environ()
	return cmd.CombinedOutput()
}

func linuxSandboxLaunchUnsupported(output string) bool {
	for _, marker := range []string{
		"Operation not permitted",
		"Permission denied",
		"Invalid argument",
		"No permissions to create new namespace",
		"creating new namespace failed",
		"bubblewrap is not available",
		"Can't mount proc on",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

func landlockLaunchUnsupported(output string) bool {
	for _, marker := range []string{
		"apply Landlock: query ABI",
		"operation not supported",
		"Operation not supported",
		"invalid argument",
		"Invalid argument",
		"function not implemented",
		"Function not implemented",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
