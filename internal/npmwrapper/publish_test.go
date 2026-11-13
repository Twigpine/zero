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
// It keeps one package's registry state in FAKE_NPM_STATE and answers the way
// npm 11 does: a lookup of a missing package or a missing version fails with
// E404, and it refuses a version published twice or any lookup or publish for
// a package name other than the one it holds. Every call is appended to
// FAKE_NPM_STATE.log.
//
// FAKE_NPM_CLOBBER_LATEST=1 makes every publish take `latest`, the failure the
// script must stop on. FAKE_NPM_VERSION_LOOKUP_ERRORS=<n> fails the first n
// version lookups with a network error instead of answering them.
// FAKE_NPM_APPLY_AFTER=<n> applies each publish only after n further lookups,
// the way npm accepts an upload and shows the version minutes later; until
// then the version and its dist-tag are invisible. When a wrapper is published,
// the aliased versions not yet visible are recorded in missingAtWrapperPublish.
const fakeNpm = `#!/usr/bin/env node
const fs = require('fs');
const stateFile = process.env.FAKE_NPM_STATE;
const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
const args = process.argv.slice(2);
fs.appendFileSync(stateFile + '.log', JSON.stringify({ args, cwd: process.cwd() }) + '\n');
const save = () => fs.writeFileSync(stateFile, JSON.stringify(state));
const notFound = (detail) => { process.stderr.write('npm error code E404\nnpm error 404 ' + detail + '\n'); process.exit(1); };
state.pending = state.pending || [];
const apply = (publish) => {
  state.versions.push(publish.version);
  state.distTags[publish.tag] = publish.version;
  if (process.env.FAKE_NPM_CLOBBER_LATEST === '1') state.distTags.latest = publish.version;
};
if (args[0] === 'view') {
  for (const publish of state.pending) publish.wait -= 1;
  state.pending.filter((publish) => publish.wait <= 0).forEach(apply);
  state.pending = state.pending.filter((publish) => publish.wait > 0);
  save();
  const [spec, field] = args.slice(1);
  const at = spec.lastIndexOf('@');
  const name = at > 0 ? spec.slice(0, at) : spec;
  if (field === 'version') {
    const failures = Number(process.env.FAKE_NPM_VERSION_LOOKUP_ERRORS || 0);
    state.versionLookups = (state.versionLookups || 0) + 1;
    save();
    if (state.versionLookups <= failures) {
      process.stderr.write('npm error code ECONNRESET\nnpm error network aborted\n');
      process.exit(1);
    }
  }
  if (name !== state.name || state.versions.length === 0) notFound('Not Found - GET ' + name);
  if (field === 'version') {
    const version = spec.slice(at + 1);
    if (!state.versions.includes(version)) notFound('No match found for version ' + version);
    process.stdout.write(version + '\n');
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
  if (state.versions.includes(pkg.version) || state.pending.some((publish) => publish.version === pkg.version)) {
    process.stderr.write('npm error code EPUBLISHCONFLICT\n');
    process.exit(1);
  }
  if (pkg.optionalDependencies) {
    state.missingAtWrapperPublish = Object.values(pkg.optionalDependencies)
      .map((spec) => spec.slice(spec.lastIndexOf('@') + 1))
      .filter((version) => !state.versions.includes(version));
  }
  const tagIndex = args.indexOf('--tag');
  const publish = { version: pkg.version, tag: tagIndex === -1 ? 'latest' : args[tagIndex + 1], wait: Number(process.env.FAKE_NPM_APPLY_AFTER || 0) };
  if (publish.wait > 0) state.pending.push(publish);
  else apply(publish);
  save();
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
	// VersionLookups counts `npm view <name>@<version> version` calls.
	VersionLookups int `json:"versionLookups,omitempty"`
	// MissingAtWrapperPublish lists the platform versions the wrapper aliased
	// that npm did not show yet when the wrapper was published.
	MissingAtWrapperPublish []string `json:"missingAtWrapperPublish,omitempty"`
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

// A lookup that fails for a reason other than E404 must not read as "not
// published": on the re-run this skip exists for, the version is usually out
// already, and publishing it again fails with EPUBLISHCONFLICT.
func TestPublishPackagesRetriesAFailedLookupInsteadOfPublishing(t *testing.T) {
	platforms := []string{"linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{
		Name:     publishFixtureName,
		Versions: []string{publishFixtureVersion + "-linux-x64"},
		DistTags: map[string]string{"platform": publishFixtureVersion + "-linux-x64"},
	}, "FAKE_NPM_VERSION_LOOKUP_ERRORS=2")
	if run.exitCode != 0 {
		t.Fatalf("exit = %d after two transient lookup failures; stdout=%s stderr=%s", run.exitCode, run.stdout, run.stderr)
	}
	if got := strings.Join(run.publishedDirs(), ","); got != "wrapper" {
		t.Fatalf("published %s, want only the wrapper (the platform version was already out)", got)
	}
}

// When the registry cannot answer at all, nothing may be published: the
// script cannot tell which versions are already out.
func TestPublishPackagesFailsWhenTheRegistryCannotBeReached(t *testing.T) {
	platforms := []string{"linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName}, "FAKE_NPM_VERSION_LOOKUP_ERRORS=1000")
	if run.exitCode == 0 {
		t.Fatalf("exit = 0 with the registry unreachable; stdout=%s", run.stdout)
	}
	if len(run.publishes) != 0 {
		t.Fatalf("published %v without being able to look anything up, want nothing", run.publishedDirs())
	}
	if !strings.Contains(run.stderr, "could not look up") || !strings.Contains(run.stderr, "ECONNRESET") {
		t.Fatalf("stderr does not explain the failure or show npm's error: %s", run.stderr)
	}
}

// npm accepts an upload and shows the version minutes later (ten minutes for a
// 0.10.0 platform payload). Publishing the wrapper as soon as the platform
// uploads return would leave installs resolving aliases that 404, and checking
// `latest` right after the wrapper upload reports a failure that is not one.
func TestPublishPackagesWaitsForNpmToApplyEachPublish(t *testing.T) {
	platforms := []string{"darwin-arm64", "linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName}, "FAKE_NPM_APPLY_AFTER=8")
	if run.exitCode != 0 {
		t.Fatalf("exit = %d while npm applied the publishes late; stdout=%s stderr=%s", run.exitCode, run.stdout, run.stderr)
	}
	if len(run.state.MissingAtWrapperPublish) != 0 {
		t.Fatalf("the wrapper went out while npm did not show %v yet; installs would resolve aliases that 404", run.state.MissingAtWrapperPublish)
	}
	if latest := run.state.DistTags["latest"]; latest != publishFixtureVersion {
		t.Fatalf("latest = %q, want %q", latest, publishFixtureVersion)
	}
}

// A publish npm never applies must fail the job at the deadline, before the
// wrapper goes out, instead of waiting forever.
func TestPublishPackagesGivesUpWhenNpmNeverAppliesAPublish(t *testing.T) {
	platforms := []string{"linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName},
		"FAKE_NPM_APPLY_AFTER=1000000", "ZERO_NPM_PUBLISH_WAIT_TIMEOUT=1")
	if run.exitCode == 0 {
		t.Fatalf("exit = 0 although npm never showed the platform version; stdout=%s", run.stdout)
	}
	if got := strings.Join(run.publishedDirs(), ","); got != "zero-linux-x64" {
		t.Fatalf("published %s, want only the platform payload (the wrapper must wait for it)", got)
	}
	if !strings.Contains(run.stderr, "still does not show "+publishFixtureName+"@"+publishFixtureVersion+"-linux-x64") {
		t.Fatalf("stderr does not say what npm never showed: %s", run.stderr)
	}
}

// Bash arithmetic reads a leading zero as octal and rejects 08 outright, so
// the timeout has to be converted explicitly rather than used as typed. The
// rejection does not stop a script: it prints an error, leaves the deadline
// unset and carries on, so the failure only shows once the script has to wait,
// after the platform payloads are out. npm applying late makes it wait here.
func TestPublishPackagesReadsTheWaitTimeoutAsDecimal(t *testing.T) {
	platforms := []string{"linux-x64"}
	npmDir := writePublishFixture(t, platforms, platforms)
	run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName},
		"ZERO_NPM_PUBLISH_WAIT_TIMEOUT=08", "FAKE_NPM_APPLY_AFTER=3")
	if run.exitCode != 0 {
		t.Fatalf("exit = %d with ZERO_NPM_PUBLISH_WAIT_TIMEOUT=08; stdout=%s stderr=%s", run.exitCode, run.stdout, run.stderr)
	}
	if got := strings.Join(run.publishedDirs(), ","); got != "zero-linux-x64,wrapper" {
		t.Fatalf("published %s, want zero-linux-x64,wrapper", got)
	}
}

// A setting the script cannot use must stop it before anything is published:
// a negative timeout would otherwise end the wait before it starts, and a bad
// delay would only fail at the first sleep, after the platform payloads.
func TestPublishPackagesRejectsAnInvalidWaitSetting(t *testing.T) {
	for _, tc := range []struct{ variable, value string }{
		{"ZERO_NPM_PUBLISH_WAIT_TIMEOUT", "-5"},
		{"ZERO_NPM_PUBLISH_WAIT_TIMEOUT", "abc"},
		{"ZERO_NPM_PUBLISH_WAIT_TIMEOUT", "1e3"},
		{"ZERO_NPM_PUBLISH_WAIT_TIMEOUT", "86401"},
		{"ZERO_NPM_PUBLISH_WAIT_TIMEOUT", "9999999999999999999999"},
		{"ZERO_NPM_PUBLISH_RETRY_DELAY", "soon"},
	} {
		t.Run(tc.variable+"="+tc.value, func(t *testing.T) {
			platforms := []string{"linux-x64"}
			npmDir := writePublishFixture(t, platforms, platforms)
			run := runPublishPackages(t, npmDir, fakeRegistryState{Name: publishFixtureName}, tc.variable+"="+tc.value)
			if run.exitCode == 0 {
				t.Fatalf("exit = 0 with %s=%s; stdout=%s", tc.variable, tc.value, run.stdout)
			}
			if len(run.publishes) != 0 {
				t.Fatalf("published %v before rejecting %s=%s, want nothing", run.publishedDirs(), tc.variable, tc.value)
			}
			if !strings.Contains(run.stderr, tc.variable) {
				t.Fatalf("stderr does not name %s: %s", tc.variable, run.stderr)
			}
		})
	}
}
