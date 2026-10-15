package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNormalizeVersionTagAndCompare(t *testing.T) {
	got, err := NormalizeVersionTag("v1.2.3+build.4")
	if err != nil {
		t.Fatalf("NormalizeVersionTag returned error: %v", err)
	}
	if got != "1.2.3" {
		t.Fatalf("NormalizeVersionTag = %q, want 1.2.3", got)
	}

	comparison, err := CompareSemver("0.2.0", "0.1.9")
	if err != nil {
		t.Fatalf("CompareSemver returned error: %v", err)
	}
	if comparison <= 0 {
		t.Fatal("0.2.0 should be newer than 0.1.9")
	}

	comparison, err = CompareSemver("v0.1.0", "0.1.0")
	if err != nil {
		t.Fatalf("CompareSemver returned error: %v", err)
	}
	if comparison != 0 {
		t.Fatal("v0.1.0 should match 0.1.0")
	}
}

func TestNormalizeVersionTagAndCompareReportInvalidInput(t *testing.T) {
	if _, err := NormalizeVersionTag("nightly"); err == nil {
		t.Fatal("NormalizeVersionTag should reject invalid versions")
	}
	if _, err := NormalizeVersionTag("v999999999999999999999.0.0"); err == nil {
		t.Fatal("NormalizeVersionTag should reject oversized version components")
	}
	if _, err := CompareSemver("0.2.0", "nightly"); err == nil {
		t.Fatal("CompareSemver should reject invalid versions")
	}
	if _, err := CompareSemver("v999999999999999999999.0.0", "0.1.0"); err == nil {
		t.Fatal("CompareSemver should reject oversized version components")
	}
}

func TestCheckReportsAvailableUpdate(t *testing.T) {
	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		Fetch: func(_ context.Context, endpoint string) (Release, error) {
			if endpoint != Endpoint(DefaultRepository) {
				t.Fatalf("endpoint = %q, want default", endpoint)
			}
			return releaseForTarget(t, "v0.2.0", "linux", "amd64"), nil
		},
	})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.UpdateAvailable || result.LatestVersion != "0.2.0" {
		t.Fatalf("unexpected update result: %#v", result)
	}
	if !result.ReleaseAsset.Verified || result.ReleaseAsset.ArchiveName != "zero-v0.2.0-linux-x64.tar.gz" || result.ReleaseAsset.ChecksumName != "zero-v0.2.0-linux-x64.tar.gz.sha256" {
		t.Fatalf("unexpected release asset check: %#v", result.ReleaseAsset)
	}
}

func TestResolveTarget(t *testing.T) {
	tests := []struct {
		input    string
		name     string
		goos     string
		goarch   string
		platform string
		arch     string
	}{
		{input: "linux-x64", name: "linux-x64", goos: "linux", goarch: "amd64", platform: "linux", arch: "x64"},
		{input: " Linux-X64 ", name: "linux-x64", goos: "linux", goarch: "amd64", platform: "linux", arch: "x64"},
		{input: "linux-arm64", name: "linux-arm64", goos: "linux", goarch: "arm64", platform: "linux", arch: "arm64"},
		{input: "macos-x64", name: "macos-x64", goos: "darwin", goarch: "amd64", platform: "macos", arch: "x64"},
		{input: "MacOS-Arm64", name: "macos-arm64", goos: "darwin", goarch: "arm64", platform: "macos", arch: "arm64"},
		{input: "windows-x64", name: "windows-x64", goos: "windows", goarch: "amd64", platform: "windows", arch: "x64"},
		{input: " WINDOWS-ARM64 ", name: "windows-arm64", goos: "windows", goarch: "arm64", platform: "windows", arch: "arm64"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			target, err := ResolveTarget(tt.input)
			if err != nil {
				t.Fatalf("ResolveTarget returned error: %v", err)
			}
			if target.Name != tt.name || target.GOOS != tt.goos || target.GOARCH != tt.goarch || target.Platform != tt.platform || target.Arch != tt.arch {
				t.Fatalf("ResolveTarget(%q) = %#v", tt.input, target)
			}
		})
	}

	if _, err := ResolveTarget("solaris-sparc"); err == nil || !strings.Contains(err.Error(), "unsupported update target") {
		t.Fatalf("ResolveTarget invalid error = %v, want unsupported target", err)
	}
}

func TestCheckReturnsFetchError(t *testing.T) {
	wantErr := errors.New("network failure")

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Fetch: func(context.Context, string) (Release, error) {
			return Release{}, wantErr
		},
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("Check error = %v, want %v", err, wantErr)
	}
}

func TestCheckRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := Check(ctx, Options{
		CurrentVersion: "0.1.0",
		Fetch: func(ctx context.Context, _ string) (Release, error) {
			<-ctx.Done()
			return Release{}, ctx.Err()
		},
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Check error = %v, want context deadline", err)
	}
}

func TestCheckRejectsNegativeTimeout(t *testing.T) {
	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Timeout:        -time.Second,
		Fetch: func(context.Context, string) (Release, error) {
			t.Fatal("Fetch should not run for invalid timeout")
			return Release{}, nil
		},
	})

	if err == nil || !strings.Contains(err.Error(), "timeout must be non-negative") {
		t.Fatalf("Check error = %v, want non-negative timeout error", err)
	}
}

func TestCheckRejectsMissingTagName(t *testing.T) {
	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Fetch: func(context.Context, string) (Release, error) {
			return Release{HTMLURL: "https://example.test/release"}, nil
		},
	})

	if err == nil || !strings.Contains(err.Error(), "tag_name") {
		t.Fatalf("Check error = %v, want missing tag_name error", err)
	}
}

func TestCheckRejectsInvalidLatestVersion(t *testing.T) {
	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Fetch: func(context.Context, string) (Release, error) {
			return Release{TagName: "nightly"}, nil
		},
	})

	if err == nil || !strings.Contains(err.Error(), "invalid semantic version") {
		t.Fatalf("Check error = %v, want invalid version error", err)
	}
}

func TestCheckFallsBackReleaseURL(t *testing.T) {
	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Repository:     "Gitlawb/zero",
		GOOS:           "linux",
		GOARCH:         "amd64",
		Fetch: func(context.Context, string) (Release, error) {
			release := releaseForTarget(t, "v0.2.0", "linux", "amd64")
			release.HTMLURL = ""
			return release, nil
		},
	})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}

	wantURL := "https://github.com/Gitlawb/zero/releases/tag/v0.2.0"
	if result.ReleaseURL != wantURL {
		t.Fatalf("ReleaseURL = %q, want %q", result.ReleaseURL, wantURL)
	}
}

func TestCheckFetchesDataEndpoint(t *testing.T) {
	payload := url.QueryEscape(`{"tag_name":"v0.2.0","html_url":"https://example.test/release","assets":[{"name":"zero-v0.2.0-linux-x64.tar.gz","browser_download_url":"https://example.test/zero-v0.2.0-linux-x64.tar.gz"},{"name":"zero-v0.2.0-linux-x64.tar.gz.sha256","browser_download_url":"https://example.test/zero-v0.2.0-linux-x64.tar.gz.sha256"}]}`)

	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Endpoint:       "data:application/json," + payload,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !result.UpdateAvailable || result.ReleaseURL != "https://example.test/release" || !result.ReleaseAsset.Verified {
		t.Fatalf("unexpected data endpoint result: %#v", result)
	}
}

func TestCheckResolvesEndpointPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		options  Options
		env      string
		want     string
		clearEnv bool
	}{
		{
			name:    "endpoint option wins",
			options: Options{Endpoint: "Gitlawb/option-zero", Repository: "Gitlawb/repo-zero"},
			env:     "Gitlawb/env-zero",
			want:    Endpoint("Gitlawb/option-zero"),
		},
		{
			name:    "environment wins over repository",
			options: Options{Repository: "Gitlawb/repo-zero"},
			env:     "Gitlawb/env-zero",
			want:    Endpoint("Gitlawb/env-zero"),
		},
		{
			name:     "repository wins over default",
			options:  Options{Repository: "Gitlawb/repo-zero"},
			want:     Endpoint("Gitlawb/repo-zero"),
			clearEnv: true,
		},
		{
			name:     "default repository last",
			want:     Endpoint(DefaultRepository),
			clearEnv: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.clearEnv {
				t.Setenv("ZERO_UPDATE_RELEASE_URL", "")
			} else {
				t.Setenv("ZERO_UPDATE_RELEASE_URL", tt.env)
			}
			options := tt.options
			options.CurrentVersion = "0.1.0"
			options.GOOS = "linux"
			options.GOARCH = "amd64"
			options.Fetch = func(_ context.Context, endpoint string) (Release, error) {
				if endpoint != tt.want {
					t.Fatalf("endpoint = %q, want %q", endpoint, tt.want)
				}
				return releaseForTarget(t, "v0.2.0", "linux", "amd64"), nil
			}

			if _, err := Check(context.Background(), options); err != nil {
				t.Fatalf("Check returned error: %v", err)
			}
		})
	}
}

func TestCheckRejectsMissingReleaseAssets(t *testing.T) {
	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		Fetch: func(context.Context, string) (Release, error) {
			return Release{
				TagName: "v0.2.0",
				Assets: []Asset{
					{Name: "zero-v0.2.0-linux-arm64.tar.gz"},
				},
			}, nil
		},
	})

	if err == nil {
		t.Fatal("Check should reject release metadata without expected assets")
	}
	if !strings.Contains(err.Error(), "zero-v0.2.0-linux-x64.tar.gz") || !strings.Contains(err.Error(), "zero-v0.2.0-linux-x64.tar.gz.sha256") {
		t.Fatalf("Check error = %v, want missing archive and checksum names", err)
	}
}

func TestExpectedAssetCheckUsesInstallerArchiveNames(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		goos        string
		goarch      string
		archiveName string
		platform    string
		arch        string
	}{
		{
			name:        "linux amd64",
			version:     "0.2.0",
			goos:        "linux",
			goarch:      "amd64",
			archiveName: "zero-v0.2.0-linux-x64.tar.gz",
			platform:    "linux",
			arch:        "x64",
		},
		{
			name:        "macos arm64",
			version:     "0.2.0",
			goos:        "darwin",
			goarch:      "arm64",
			archiveName: "zero-v0.2.0-macos-arm64.tar.gz",
			platform:    "macos",
			arch:        "arm64",
		},
		{
			name:        "windows amd64",
			version:     "0.2.0",
			goos:        "windows",
			goarch:      "amd64",
			archiveName: "zero-v0.2.0-windows-x64.zip",
			platform:    "windows",
			arch:        "x64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check, err := expectedAssetCheck(tt.version, tt.goos, tt.goarch)
			if err != nil {
				t.Fatalf("expectedAssetCheck returned error: %v", err)
			}
			if check.ArchiveName != tt.archiveName || check.ChecksumName != tt.archiveName+".sha256" || check.Platform != tt.platform || check.Arch != tt.arch {
				t.Fatalf("expectedAssetCheck = %#v, want archive %s", check, tt.archiveName)
			}
		})
	}
}

func TestCheckReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Endpoint:       server.URL,
	})

	if err == nil || !strings.Contains(err.Error(), "github release check failed") {
		t.Fatalf("Check error = %v, want HTTP status error", err)
	}
}

func TestCheckReportsInvalidHTTPJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.1.0",
		Endpoint:       server.URL,
	})

	if err == nil {
		t.Fatal("Check should reject invalid JSON")
	}
}

func TestResolveEndpointAcceptsURLAndRepositorySlug(t *testing.T) {
	got, err := ResolveEndpoint("Gitlawb/alt-zero", DefaultRepository)
	if err != nil {
		t.Fatalf("ResolveEndpoint returned error: %v", err)
	}
	if got != Endpoint("Gitlawb/alt-zero") {
		t.Fatalf("slug endpoint = %q", got)
	}

	got, err = ResolveEndpoint("https://example.test/latest", DefaultRepository)
	if err != nil {
		t.Fatalf("ResolveEndpoint returned error: %v", err)
	}
	if got != "https://example.test/latest" {
		t.Fatalf("URL endpoint = %q", got)
	}

	got, err = ResolveEndpoint("", "Gitlawb/fallback")
	if err != nil {
		t.Fatalf("ResolveEndpoint returned error: %v", err)
	}
	if got != Endpoint("Gitlawb/fallback") {
		t.Fatalf("fallback endpoint = %q", got)
	}
}

func TestResolveEndpointRejectsInvalidInput(t *testing.T) {
	_, err := ResolveEndpoint("not a url", DefaultRepository)

	if err == nil || !strings.Contains(err.Error(), "invalid update endpoint") {
		t.Fatalf("ResolveEndpoint error = %v, want invalid endpoint error", err)
	}
}

func TestFormatResult(t *testing.T) {
	output := Format(Result{
		CurrentVersion:  "0.1.0",
		LatestVersion:   "0.2.0",
		ReleaseURL:      "https://github.com/Gitlawb/zero/releases/tag/v0.2.0",
		TagName:         "v0.2.0",
		ReleaseAsset:    assetCheckForTest(t, "v0.2.0", "linux", "amd64"),
		UpdateAvailable: true,
	})
	if !strings.Contains(output, "Update available: 0.1.0 -> 0.2.0") {
		t.Fatalf("unexpected update output: %q", output)
	}
	if !strings.Contains(output, "Release asset: zero-v0.2.0-linux-x64.tar.gz") || !strings.Contains(output, "Checksum asset: zero-v0.2.0-linux-x64.tar.gz.sha256") {
		t.Fatalf("update output did not include release assets: %q", output)
	}
	if !strings.Contains(output, "Release target: linux-x64") {
		t.Fatalf("update output did not include target-specific guidance: %q", output)
	}
	if strings.Contains(output, "your platform") {
		t.Fatalf("update output should not use ambiguous platform wording: %q", output)
	}

	// The upgrade recommendation is about THIS machine, so it depends on whether
	// the checked target is this machine — assert it against the local target
	// rather than the fixed linux-x64 fixture above, which is a cross-target
	// check anywhere but linux/amd64.
	local := Format(Result{
		CurrentVersion:  "0.1.0",
		LatestVersion:   "0.2.0",
		ReleaseURL:      "https://github.com/Gitlawb/zero/releases/tag/v0.2.0",
		TagName:         "v0.2.0",
		ReleaseAsset:    assetCheckForTest(t, "v0.2.0", runtime.GOOS, runtime.GOARCH),
		UpdateAvailable: true,
	})
	if !strings.Contains(local, "Run `zero upgrade` to download, verify, and install") {
		t.Fatalf("current-platform check did not recommend zero upgrade: %q", local)
	}

	output = Format(Result{
		CurrentVersion:  "0.2.0",
		LatestVersion:   "0.2.0",
		ReleaseURL:      "https://github.com/Gitlawb/zero/releases/tag/v0.2.0",
		TagName:         "v0.2.0",
		ReleaseAsset:    assetCheckForTest(t, "v0.2.0", "linux", "amd64"),
		UpdateAvailable: false,
	})
	if !strings.Contains(output, "up to date") {
		t.Fatalf("unexpected up-to-date output: %q", output)
	}
	if !strings.Contains(output, "Release target: linux-x64") {
		t.Fatalf("up-to-date output did not include release target: %q", output)
	}
}

// TestFormatCustomSourceCheckRepeatsTheSourceFlag covers jatmn's #489 finding:
// `zero upgrade` is a fresh invocation that reads from the DEFAULT source, so
// recommending it bare after `--check --repo <fork>` sends the user to install
// from a repository they never asked about. The pre-PR text pointed at the
// printed asset URLs instead; this keeps a runnable command by naming the flag
// to repeat.
func TestFormatCustomSourceCheckRepeatsTheSourceFlag(t *testing.T) {
	local := assetCheckForTest(t, "v0.2.0", runtime.GOOS, runtime.GOARCH)
	for _, tc := range []struct {
		name    string
		options Options
		want    string
	}{
		{name: "repo flag", options: Options{Repository: "someone/fork"}, want: "zero upgrade --repo someone/fork"},
		{name: "endpoint flag", options: Options{Endpoint: "https://example.test/releases/latest?channel=$preview"}, want: "zero upgrade --endpoint 'https://example.test/releases/latest?channel=$preview'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := Format(Result{
				CurrentVersion:  "0.1.0",
				LatestVersion:   "0.2.0",
				ReleaseURL:      "https://example.test/release",
				TagName:         "v0.2.0",
				ReleaseAsset:    local,
				UpdateAvailable: true,
				SourceFlag:      upgradeSourceFlag(tc.options),
			})
			if !strings.Contains(output, tc.want) {
				t.Fatalf("output = %q, want it to repeat %q", output, tc.want)
			}
			if !strings.Contains(output, "bare `zero upgrade` does not repeat") {
				t.Fatalf("output = %q, want it to distinguish a bare upgrade", output)
			}
		})
	}

	// The default source keeps the plain call to action, including when --repo
	// names the default repository explicitly.
	for _, options := range []Options{{}, {Repository: DefaultRepository}} {
		output := Format(Result{
			CurrentVersion:  "0.1.0",
			LatestVersion:   "0.2.0",
			ReleaseURL:      "https://example.test/release",
			TagName:         "v0.2.0",
			ReleaseAsset:    local,
			UpdateAvailable: true,
			SourceFlag:      upgradeSourceFlag(options),
		})
		if !strings.Contains(output, "Run `zero upgrade` to download") {
			t.Fatalf("default-source output = %q, want the plain upgrade recommendation", output)
		}
	}
}

// TestCheckRecordsTheSourceFlagItWasGiven pins where SourceFlag comes from: the
// per-invocation flags, and not ZERO_UPDATE_RELEASE_URL — the env var is read by
// every Check including the one inside Apply, so a bare `zero upgrade` already
// follows it and telling the user to repeat it would be wrong.
func TestCheckRecordsTheSourceFlagItWasGiven(t *testing.T) {
	t.Setenv("ZERO_UPDATE_RELEASE_URL", "")
	fetch := func(context.Context, string) (Release, error) {
		return releaseForTarget(t, "v0.2.0", "linux", "amd64"), nil
	}
	base := Options{CurrentVersion: "0.1.0", GOOS: "linux", GOARCH: "amd64", Fetch: fetch}

	withRepo := base
	withRepo.Repository = "someone/fork"
	result, err := Check(context.Background(), withRepo)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SourceFlag != "--repo someone/fork" {
		t.Fatalf("SourceFlag = %q, want the repo flag recorded", result.SourceFlag)
	}

	result, err = Check(context.Background(), base)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SourceFlag != "" {
		t.Fatalf("SourceFlag = %q, want empty for the default source", result.SourceFlag)
	}

	t.Setenv("ZERO_UPDATE_RELEASE_URL", "https://example.test/releases/latest")
	for _, options := range []Options{base, withRepo} {
		result, err = Check(context.Background(), options)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if result.SourceFlag != "" {
			t.Fatalf("SourceFlag = %q, want empty: the env var wins over repo and carries over on its own", result.SourceFlag)
		}
	}

	withEndpoint := withRepo
	withEndpoint.Endpoint = "https://option.test/releases/latest"
	result, err = Check(context.Background(), withEndpoint)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.SourceFlag != "--endpoint 'https://option.test/releases/latest'" {
		t.Fatalf("SourceFlag = %q, want explicit endpoint recorded", result.SourceFlag)
	}
}

func TestFormatNpmCustomSourceExplainsInstallSource(t *testing.T) {
	result := Result{
		CurrentVersion: "0.1.0", LatestVersion: "0.2.0", ReleaseURL: "https://example.test/release",
		ReleaseAsset: assetCheckForTest(t, "v0.2.0", runtime.GOOS, runtime.GOARCH), UpdateAvailable: true,
		SourceFlag: "--repo someone/fork", installMethod: InstallMethodNpm,
	}
	output := Format(result)
	if strings.Contains(output, "zero upgrade --repo") || !strings.Contains(output, "npm install -g @twigpine/zero@latest") || !strings.Contains(output, "only affects the release check and update gating") {
		t.Fatalf("npm custom-source guidance is misleading: %q", output)
	}
}

// A legacy install is told to switch whether or not an update is out: up to
// date today, the next release will not reach it, and `zero upgrade` refuses.
func TestFormatTellsALegacyNpmInstallToSwitch(t *testing.T) {
	asset := assetCheckForTest(t, "v0.11.0", runtime.GOOS, runtime.GOARCH)
	for name, result := range map[string]Result{
		"up to date": {
			CurrentVersion: "0.10.0", LatestVersion: "0.10.0", ReleaseURL: "https://example.test/release",
			ReleaseAsset: asset, installMethod: InstallMethodNpmLegacy,
		},
		"update available": {
			CurrentVersion: "0.10.0", LatestVersion: "0.11.0", ReleaseURL: "https://example.test/release",
			ReleaseAsset: asset, UpdateAvailable: true, installMethod: InstallMethodNpmLegacy,
		},
	} {
		output := Format(result)
		if !strings.Contains(output, "npm uninstall -g @gitlawb/zero") || !strings.Contains(output, "npm install -g @twigpine/zero") {
			t.Errorf("%s: output does not give the switch steps: %q", name, output)
		}
		if strings.Contains(output, "Run `zero upgrade`") {
			t.Errorf("%s: output recommends `zero upgrade`, which refuses a legacy install: %q", name, output)
		}
	}
}

func TestFormatCrossTargetCustomSourceKeepsAccurateGuidance(t *testing.T) {
	goos, goarch := "linux", "amd64"
	if runtime.GOOS == goos && runtime.GOARCH == goarch {
		goos, goarch = "darwin", "arm64"
	}
	output := Format(Result{
		CurrentVersion: "0.1.0", LatestVersion: "0.2.0", ReleaseURL: "https://example.test/release",
		ReleaseAsset: assetCheckForTest(t, "v0.2.0", goos, goarch), UpdateAvailable: true,
		SourceFlag: "--repo someone/fork",
	})
	if strings.Contains(output, "Run `zero upgrade") || !strings.Contains(output, "Download the verified") || !strings.Contains(output, "custom source selected by `--repo someone/fork`") || !strings.Contains(output, "bare `zero upgrade` does not repeat that source") {
		t.Fatalf("cross-target custom-source guidance is misleading: %q", output)
	}
}

// TestFormatCrossTargetCheckDoesNotRecommendLocalUpgrade is the regression test
// for jatmn's #489 finding: `zero update --check --target <other>` answers a
// question about a different machine, but the output recommended `zero upgrade`,
// which only ever installs onto this one. That replaced the target-specific
// manual-download guidance the flag used to print.
func TestFormatCrossTargetCheckDoesNotRecommendLocalUpgrade(t *testing.T) {
	// Pick a target that is definitely not this machine.
	goos, goarch := "linux", "amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		goos, goarch = "darwin", "arm64"
	}
	other := assetCheckForTest(t, "v0.2.0", goos, goarch)
	output := Format(Result{
		CurrentVersion:  "0.1.0",
		LatestVersion:   "0.2.0",
		ReleaseURL:      "https://github.com/Gitlawb/zero/releases/tag/v0.2.0",
		TagName:         "v0.2.0",
		ReleaseAsset:    other,
		UpdateAvailable: true,
	})
	if strings.Contains(output, "Run `zero upgrade`") {
		t.Fatalf("a cross-target check must not recommend the local upgrade: %q", output)
	}
	target := other.Platform + "-" + other.Arch
	if !strings.Contains(output, "Download the verified "+target+" release asset") {
		t.Fatalf("cross-target check lost its target-specific guidance: %q", output)
	}
	if localReleaseTarget() != "" && !strings.Contains(output, "installs onto this machine") {
		t.Fatalf("cross-target check should say which machine zero upgrade would touch: %q", output)
	}
}

func TestPublishedReleaseTargetRejectsUnsupportedCombinations(t *testing.T) {
	tests := []struct {
		goos, goarch, want string
	}{
		{goos: "linux", goarch: "amd64", want: "linux-x64"},
		{goos: "darwin", goarch: "arm64", want: "macos-arm64"},
		{goos: "windows", goarch: "amd64", want: "windows-x64"},
		{goos: "windows", goarch: "arm64", want: ""},
		{goos: "android", goarch: "arm64", want: ""},
	}
	for _, test := range tests {
		if got := publishedReleaseTarget(test.goos, test.goarch); got != test.want {
			t.Errorf("publishedReleaseTarget(%q, %q) = %q, want %q", test.goos, test.goarch, got, test.want)
		}
	}
}

func releaseForTarget(t *testing.T, tag string, goos string, goarch string) Release {
	t.Helper()
	check := assetCheckForTest(t, tag, goos, goarch)
	return Release{
		TagName: tag,
		HTMLURL: "https://github.com/Gitlawb/zero/releases/tag/" + tag,
		Assets: []Asset{
			{Name: check.ArchiveName, BrowserDownloadURL: "https://example.test/" + check.ArchiveName},
			{Name: check.ChecksumName, BrowserDownloadURL: "https://example.test/" + check.ChecksumName},
		},
	}
}

func assetCheckForTest(t *testing.T, tag string, goos string, goarch string) AssetCheck {
	t.Helper()
	version, err := NormalizeVersionTag(tag)
	if err != nil {
		t.Fatalf("NormalizeVersionTag(%q): %v", tag, err)
	}
	check, err := expectedAssetCheck(version, goos, goarch)
	if err != nil {
		t.Fatalf("expectedAssetCheck(%q, %q, %q): %v", version, goos, goarch, err)
	}
	check.ArchiveFound = true
	check.ChecksumFound = true
	check.Verified = true
	return check
}
