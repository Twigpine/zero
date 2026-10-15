package npmwrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeNpm stands in for npm on PATH when scripts/npm/publish-packages.sh runs.
// It keeps one package's registry state in FAKE_NPM_STATE and refuses what the
// real registry refuses: a version published twice, and any lookup or publish
// for a package name other than the one it holds. Every call is appended to
// FAKE_NPM_STATE.log. FAKE_NPM_CLOBBER_LATEST=1 makes every publish take
// `latest`, the failure the script must stop on.
const fakeNpm = `#!/usr/bin/env node
const fs = require('fs');
const stateFile = process.env.FAKE_NPM_STATE;
const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
const args = process.argv.slice(2);
fs.appendFileSync(stateFile + '.log', JSON.stringify({ args, cwd: process.cwd() }) + '\n');
const notFound = () => { process.stderr.write('npm error code E404\n'); process.exit(1); };
if (args[0] === 'view') {
  const [spec, field] = args.slice(1);
  const at = spec.lastIndexOf('@');
  const name = at > 0 ? spec.slice(0, at) : spec;
  if (name !== state.name || state.versions.length === 0) notFound();
  if (field === 'version') {
    const version = spec.slice(at + 1);
    if (state.versions.includes(version)) process.stdout.write(version + '\n');
    process.exit(0);
  }
  if (field === 'dist-tags.latest') {
    if (state.distTags.latest) process.stdout.write(state.distTags.latest + '\n');
    process.exit(0);
  }
}
if (args[0] === 'publish') {
  const pkg = JSON.parse(fs.readFileSync('package.json', 'utf8'));
  if (pkg.name !== state.name) {
    process.stderr.write('fake npm: refusing to publish ' + pkg.name + '\n');
    process.exit(1);
  }
  if (state.versions.includes(pkg.version)) {
    process.stderr.write('npm error code EPUBLISHCONFLICT\n');
    process.exit(1);
  }
  const tagIndex = args.indexOf('--tag');
  state.versions.push(pkg.version);
  state.distTags[tagIndex === -1 ? 'latest' : args[tagIndex + 1]] = pkg.version;
  if (process.env.FAKE_NPM_CLOBBER_LATEST === '1') state.distTags.latest = pkg.version;
  fs.writeFileSync(stateFile, JSON.stringify(state));
  process.exit(0);
}
process.stderr.write('fake npm: unsupported command ' + args.join(' ') + '\n');
process.exit(2);
`

const (
	publishFixtureName    = "@example/zero"
	publishFixtureVersion = "1.2.3"
)

type fakeRegistryState struct {
	Name     string            `json:"name"`
	Versions []string          `json:"versions"`
	DistTags map[string]string `json:"distTags"`
}

type npmCall struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
}

type publishRun struct {
	stdout, stderr string
	exitCode       int
	state          fakeRegistryState
	publishes      []npmCall
}

// writePublishFixture lays out what build-platform-packages.mjs emits: a
// wrapper whose optionalDependencies alias the given platform payloads, and
// one directory per payload in platforms.
func writePublishFixture(t *testing.T, aliasedPlatforms []string, platformDirs []string) string {
	t.Helper()
	npmDir := filepath.Join(t.TempDir(), "npm")
	aliases := map[string]string{}
	for _, platform := range aliasedPlatforms {
		aliases[publishFixtureName+"-"+platform] = "npm:" + publishFixtureName + "@" + publishFixtureVersion + "-" + platform
	}
	wrapper, err := json.Marshal(map[string]any{
		"name":                 publishFixtureName,
		"version":              publishFixtureVersion,
		"optionalDependencies": aliases,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(npmDir, "wrapper", "package.json"), string(wrapper), 0o644)
	for _, platform := range platformDirs {
		manifest := `{"name":"` + publishFixtureName + `","version":"` + publishFixtureVersion + "-" + platform + `"}`
		writeFixtureFile(t, filepath.Join(npmDir, "platforms", "zero-"+platform, "package.json"), manifest, 0o644)
	}
	return npmDir
}

func runPublishPackages(t *testing.T, npmDir string, registry fakeRegistryState, extraEnv ...string) publishRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("publish-packages.sh runs on the Linux release runner; the fake npm is a POSIX executable")
	}
	bash := requireCommand(t, "bash")
	requireNode(t)

	binDir := t.TempDir()
	writeFixtureFile(t, filepath.Join(binDir, "npm"), fakeNpm, 0o755)
	if registry.DistTags == nil {
		registry.DistTags = map[string]string{}
	}
	if registry.Versions == nil {
		registry.Versions = []string{}
	}
	stateFile := filepath.Join(t.TempDir(), "registry.json")
	initial, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, stateFile, string(initial), 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bash, filepath.Join(repoRoot(t), "scripts", "npm", "publish-packages.sh"), npmDir)
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_NPM_STATE="+stateFile,
		"ZERO_NPM_PUBLISH_RETRY_DELAY=0",
		"NODE_OPTIONS=",
	)
	command.Env = append(command.Env, extraEnv...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	run := publishRun{}
	if err := command.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("publish-packages.sh: %v; stderr=%s", err, stderr.String())
		}
		run.exitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		t.Fatalf("publish-packages.sh timed out; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	run.stdout, run.stderr = stdout.String(), stderr.String()
	unmarshalJSONFile(t, stateFile, &run.state)

	if log, err := os.Open(stateFile + ".log"); err == nil {
		defer log.Close()
		scanner := bufio.NewScanner(log)
		for scanner.Scan() {
			var call npmCall
			if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
				t.Fatalf("fake npm log line %q: %v", scanner.Text(), err)
			}
			if len(call.Args) > 0 && call.Args[0] == "publish" {
				run.publishes = append(run.publishes, call)
			}
		}
	}
	return run
}

// publishedDirs names each publish by the payload directory it ran in, in order.
func (run publishRun) publishedDirs() []string {
	dirs := []string{}
	for _, call := range run.publishes {
		dirs = append(dirs, filepath.Base(call.Cwd))
	}
	return dirs
}

func TestPublishPackagesPublishesPlatformPayloadsBeforeTheWrapper(t *testing.T) {
	platforms := []string{"darwin-arm64", "linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName})
	if run.exitCode != 0 {
		t.Fatalf("exit = %d; stdout=%s stderr=%s", run.exitCode, run.stdout, run.stderr)
	}

	if got, want := strings.Join(run.publishedDirs(), ","), "zero-darwin-arm64,zero-linux-x64,wrapper"; got != want {
		t.Fatalf("publish order = %s, want %s (the wrapper pins the platform versions, so it must go last)", got, want)
	}
	for index, call := range run.publishes {
		args := strings.Join(call.Args, " ")
		if !strings.Contains(args, "--access public") || !strings.Contains(args, "--ignore-scripts") {
			t.Fatalf("publish %d args = %q, want --access public and --ignore-scripts", index, args)
		}
		isWrapper := filepath.Base(call.Cwd) == "wrapper"
		if hasPlatformTag := strings.Contains(args, "--tag platform"); hasPlatformTag == isWrapper {
			t.Fatalf("publish %d (%s) args = %q: platform payloads need --tag platform and the wrapper must take latest", index, filepath.Base(call.Cwd), args)
		}
	}
	if latest := run.state.DistTags["latest"]; latest != publishFixtureVersion {
		t.Fatalf("latest = %q, want %q", latest, publishFixtureVersion)
	}
}

// "Re-run failed jobs" after a partial publish must finish the release rather
// than fail on the versions that already went out.
func TestPublishPackagesSkipsVersionsAlreadyOnTheRegistry(t *testing.T) {
	platforms := []string{"darwin-arm64", "linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{
		Name:     publishFixtureName,
		Versions: []string{"1.2.2", publishFixtureVersion + "-darwin-arm64"},
		DistTags: map[string]string{"latest": "1.2.2", "platform": publishFixtureVersion + "-darwin-arm64"},
	})
	if run.exitCode != 0 {
		t.Fatalf("exit = %d; stdout=%s stderr=%s", run.exitCode, run.stdout, run.stderr)
	}
	if got, want := strings.Join(run.publishedDirs(), ","), "zero-linux-x64,wrapper"; got != want {
		t.Fatalf("published %s, want %s (darwin-arm64 was already on the registry)", got, want)
	}
	if !strings.Contains(run.stdout, publishFixtureName+"@"+publishFixtureVersion+"-darwin-arm64 is already on the registry") {
		t.Fatalf("stdout does not report the skipped version: %s", run.stdout)
	}

	// Re-running once everything is out publishes nothing and still succeeds.
	rerun := runPublishPackages(t, npmDir, run.state)
	if rerun.exitCode != 0 {
		t.Fatalf("rerun exit = %d; stdout=%s stderr=%s", rerun.exitCode, rerun.stdout, rerun.stderr)
	}
	if len(rerun.publishes) != 0 {
		t.Fatalf("rerun published %v, want nothing", rerun.publishedDirs())
	}
}

func TestPublishPackagesStopsBeforeTheWrapperWhenAPlatformTookLatest(t *testing.T) {
	platforms := []string{"linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName}, "FAKE_NPM_CLOBBER_LATEST=1")
	if run.exitCode == 0 {
		t.Fatalf("exit = 0 after a platform publish took latest; stdout=%s", run.stdout)
	}
	if got := strings.Join(run.publishedDirs(), ","); got != "zero-linux-x64" {
		t.Fatalf("published %s, want only the platform payload (the wrapper must not follow a clobbered latest)", got)
	}
	if !strings.Contains(run.stderr, "clobbered") {
		t.Fatalf("stderr does not explain the failure: %s", run.stderr)
	}
}

// A wrapper that aliases a platform version with no payload beside it would
// install an alias that 404s, so nothing may be published at all.
func TestPublishPackagesRefusesAWrapperWhoseAliasesDoNotMatchThePayloads(t *testing.T) {
	npmDir := writePublishFixture(t, []string{"darwin-arm64", "linux-x64"}, []string{"linux-x64"})
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName})
	if run.exitCode == 0 {
		t.Fatalf("exit = 0 with a missing platform payload; stdout=%s", run.stdout)
	}
	if len(run.publishes) != 0 {
		t.Fatalf("published %v before noticing the mismatch, want nothing", run.publishedDirs())
	}
	if !strings.Contains(run.stderr, "do not match the platform payloads") {
		t.Fatalf("stderr does not explain the failure: %s", run.stderr)
	}
}
