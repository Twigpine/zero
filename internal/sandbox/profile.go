package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type FileSystemPolicyKind string

const (
	FileSystemRestricted   FileSystemPolicyKind = "restricted"
	FileSystemUnrestricted FileSystemPolicyKind = "unrestricted"
	FileSystemExternal     FileSystemPolicyKind = "external"
)

type PermissionProfile struct {
	FileSystem FileSystemPolicy `json:"fileSystem"`
	Network    NetworkPolicy    `json:"network"`
	Runtime    *SandboxRuntime  `json:"runtime,omitempty"`
}

type FileSystemPolicy struct {
	Kind       FileSystemPolicyKind `json:"kind"`
	ReadRoots  []string             `json:"readRoots,omitempty"`
	WriteRoots []WritableRoot       `json:"writeRoots,omitempty"`
	DenyRead   []string             `json:"denyRead,omitempty"`
	// DenyReadIfExists contains best-effort baseline paths. Backends with
	// path-based policies can protect future paths; mount-based Linux only
	// masks optional entries that exist when the namespace is assembled.
	DenyReadIfExists []string `json:"denyReadIfExists,omitempty"`
	// MandatoryDenyReadPaths is the exact subset of DenyReadIfExists that names
	// the selected remote-daemon bearer token. Unlike optional credential-store
	// candidates, these paths must not disappear from a mount-based plan merely
	// because rotation has temporarily removed the file.
	MandatoryDenyReadPaths []string `json:"mandatoryDenyReadPaths,omitempty"`
	// DenyReadCarveouts are subtrees that stay readable INSIDE a denied root.
	// They exist so a directory-level credential deny can also cover the files
	// a store publishes (arbitrary temporary names, files created later in the
	// session) without hiding the supported non-secret subtrees that live in the
	// same directory — Zero's user plugin/specialist/command roots, whose
	// commands and scripts are themselves executed through the sandbox.
	DenyReadCarveouts []string `json:"denyReadCarveouts,omitempty"`
	// EnsureDenyReadDirs are directories Zero owns that a mount-based backend
	// may create (0700) so a mask exists for them. bubblewrap cannot mount over
	// a path that is absent when the namespace is assembled, so without this a
	// store created mid-session would be readable by an already-running sandbox.
	EnsureDenyReadDirs []string `json:"ensureDenyReadDirs,omitempty"`
	// ProcessTrustedDenyReadFiles are final credential-store pathnames derived
	// only from Zero's own process environment. Mount-based Linux cannot
	// durably deny an individual pathname because an atomic rename can replace
	// and detach the mount; its planner must fail closed while any remain after
	// profile finalization. Pathname-policy backends enforce these normally.
	ProcessTrustedDenyReadFiles []string `json:"processTrustedDenyReadFiles,omitempty"`
	// CommandDenyReadFinalFiles are the same kind of final credential-store
	// pathname, derived instead from a command-supplied environment (including
	// MCP-injected env). They stay separate from the process-trusted list
	// because they earn none of its host-side privileges: no carveouts, and
	// never an EnsureDenyReadDir, since a command must not be able to make Zero
	// create directories on the host. What they DO share is durability. A
	// bubblewrap mask over one of these is bypassable by the same atomic rename,
	// and an absent one is skipped entirely, so a backend that cannot deny them
	// durably must refuse the command rather than run it behind a mask that does
	// not hold. Pathname-policy backends enforce them normally, absent or not.
	CommandDenyReadFinalFiles []string `json:"commandDenyReadFinalFiles,omitempty"`
	// CommandDenyReadDirs are directory-shaped credential roots derived from a
	// command-supplied environment. Existing directories can be masked without
	// host mutation; a mount-based backend must fail closed when one is absent,
	// because creating an arbitrary command-selected host directory is unsafe.
	CommandDenyReadDirs  []string `json:"commandDenyReadDirs,omitempty"`
	DenyWrite            []string `json:"denyWrite,omitempty"`
	IncludePlatformRoots bool     `json:"includePlatformRoots,omitempty"`
	AllowTemp            bool     `json:"allowTemp,omitempty"`
}

type WritableRoot struct {
	Root                   string   `json:"root"`
	ReadOnlySubpaths       []string `json:"readOnlySubpaths,omitempty"`
	ProtectedMetadataNames []string `json:"protectedMetadataNames,omitempty"`
}

type NetworkPolicy struct {
	Mode NetworkMode `json:"mode"`
}

// protectedMetadataNames marks control-plane directories where the app-level
// auto-allow gate (see relativePathTouchesProtectedMetadata in engine.go)
// always requires a prompt for direct file-tool writes (write_file, edit_file,
// apply_patch): hand-editing git's objects/refs/index or Zero's own state
// bypasses git's and Zero's own consistency checks, regardless of subpath.
var protectedMetadataNames = []string{".git", ".zero", ".agents"}

// sandboxFullyProtectedMetadataNames are the metadata directories the OS-level
// sandbox write-denies in full for shell-executed commands. .git is
// deliberately excluded here: git subprocesses (fetch, commit, add, merge,
// pull, stash, ...) need to write objects, refs, the index, and FETCH_HEAD,
// and those writes go through git's own invariants, unlike a raw file-tool
// write. Only .git/hooks (auto-executing scripts) and .git/config (remote
// URLs, credential.helper, core.hooksPath) stay write-denied, via
// gitMetadataWriteCarveouts below.
var sandboxFullyProtectedMetadataNames = []string{".zero", ".agents"}

// gitMetadataWriteCarveouts returns the .git paths that stay write-denied under
// the OS-level sandbox even though the rest of .git is writable to git
// subprocesses. Worktrees and submodules store .git as a regular pointer file,
// so they protect that file itself: constructing .git/hooks below it makes
// bwrap fail before the command starts. All other entry and error states retain
// the legacy child paths. The pointer is deliberately not parsed here;
// repository-controlled metadata must not redirect sandbox rules elsewhere.
func gitMetadataWriteCarveouts(root string) []string {
	return gitMetadataWriteCarveoutsWithLstat(root, os.Lstat)
}

func gitMetadataWriteCarveoutsWithLstat(root string, lstat func(string) (os.FileInfo, error)) []string {
	gitPath := filepath.Join(root, ".git")
	children := []string{
		filepath.Join(gitPath, "hooks"),
		filepath.Join(gitPath, "config"),
	}
	info, err := lstat(gitPath)
	if err != nil || !info.Mode().IsRegular() {
		return children
	}
	return []string{gitPath}
}

func PermissionProfileFromPolicy(workspaceRoot string, policy Policy, scope *Scope) PermissionProfile {
	return permissionProfileFromPolicy(workspaceRoot, policy, scope, "", nil)
}

func permissionProfileFromPolicy(workspaceRoot string, policy Policy, scope *Scope, credentialCommandDir string, credentialEnv []string) PermissionProfile {
	if policy.Mode == "" {
		policy = DefaultPolicy()
	}
	if policy.Mode == ModeDisabled {
		return PermissionProfile{
			FileSystem: FileSystemPolicy{Kind: FileSystemUnrestricted, IncludePlatformRoots: true, AllowTemp: true},
			Network:    NetworkPolicy{Mode: NetworkAllow},
		}
	}

	roots := permissionProfileRoots(workspaceRoot, scope)
	if extra := normalizeProfileDirs(policy.AllowWrite); len(extra) > 0 {
		roots = dedupeStrings(append(roots, extra...))
	}
	readRoots := permissionProfileReadRoots(workspaceRoot, policy, scope, roots)
	writeRoots := make([]WritableRoot, 0, len(roots))
	for _, root := range roots {
		writeRoots = append(writeRoots, WritableRoot{
			Root:                   root,
			ReadOnlySubpaths:       gitMetadataWriteCarveouts(root),
			ProtectedMetadataNames: append([]string{}, sandboxFullyProtectedMetadataNames...),
		})
	}
	userDenyRead := normalizeProfilePaths(policy.DenyRead)
	commandAllowedRoots := append([]string{}, roots...)
	for _, root := range readRoots {
		if root != profileRootPath() {
			commandAllowedRoots = append(commandAllowedRoots, root)
		}
	}
	credentials := credentialDenyReadPaths(policy, credentialCommandDir, credentialEnv, dedupeStrings(commandAllowedRoots))
	credentials = finalizeCredentialDenyPaths(credentials, userDenyRead)
	return PermissionProfile{
		FileSystem: FileSystemPolicy{
			Kind:                        FileSystemRestricted,
			ReadRoots:                   readRoots,
			WriteRoots:                  writeRoots,
			DenyRead:                    userDenyRead,
			DenyReadIfExists:            credentials.Paths,
			MandatoryDenyReadPaths:      credentials.MandatoryPaths,
			DenyReadCarveouts:           credentials.Carveouts,
			EnsureDenyReadDirs:          credentials.EnsureDirs,
			ProcessTrustedDenyReadFiles: credentials.ProcessTrustedFinalFiles,
			CommandDenyReadFinalFiles:   credentials.CommandFinalFiles,
			CommandDenyReadDirs:         credentials.CommandDirs,
			DenyWrite:                   normalizeProfilePaths(policy.DenyWrite),
			IncludePlatformRoots:        true,
			AllowTemp:                   true,
		},
		Network: NetworkPolicy{Mode: NormalizeNetworkMode(policy.Network)},
	}
}

func (profile PermissionProfile) RequiresPlatformSandbox() bool {
	if profile.FileSystem.Kind == FileSystemRestricted {
		return true
	}
	return NormalizeNetworkMode(profile.Network.Mode) == NetworkDeny
}

func permissionProfileRoots(workspaceRoot string, scope *Scope) []string {
	if scope != nil {
		return scope.Roots()
	}
	var roots []string
	if root := normalizeProfilePath(workspaceRoot); root != "" {
		roots = append(roots, root)
	}
	roots = append(roots, defaultTempWriteRoots()...)
	return dedupeStrings(roots)
}

func permissionProfileReadRoots(workspaceRoot string, policy Policy, scope *Scope, writeRoots []string) []string {
	// Workspace-write follows the upstream sandbox model: full disk is readable,
	// while writes are narrowed to workspace/extra roots below. This is a
	// deliberate read-all/write-jail posture; callers that must hide secrets use
	// DenyRead to carve them out.
	readRoots := []string{profileRootPath()}
	readRoots = append(readRoots, writeRoots...)
	if scope != nil {
		readRoots = dedupeStrings(append(readRoots, scope.ReadRoots()...))
	} else if root := normalizeProfilePath(workspaceRoot); root != "" {
		readRoots = dedupeStrings(append(readRoots, root))
	}
	if extra := normalizeProfileDirs(policy.AllowRead); len(extra) > 0 {
		readRoots = dedupeStrings(append(readRoots, extra...))
	}
	return dedupeStrings(readRoots)
}

// credentialDenyPaths is the credential baseline a profile derives from the
// environment: the paths to deny reads on, the known directory paths among
// them, the trusted non-secret subtrees that stay readable, and the trusted
// Zero-owned directories a mount-based backend may create so its mask exists.
type credentialDenyPaths struct {
	Paths                    []string
	MandatoryPaths           []string
	Carveouts                []string
	EnsureDirs               []string
	Dirs                     []string
	ProcessTrustedFinalFiles []string
	// CommandFinalFiles are final token pathnames derived from a
	// command-supplied environment. Kept apart from the trusted list so they
	// never gain its host-side privileges, but tracked so a backend that cannot
	// deny a replaceable pathname can fail closed on them too.
	CommandFinalFiles []string
	CommandDirs       []string
}

// credentialDenyReadPaths returns default deny-read entries for well-known
// credential stores, including tool configuration files discoverable through
// the preserved caller environment, Zero's own config/token stores, and the
// selected daemon remote token file. Four deliberate limits:
//
//   - Windows is skipped: a non-empty profile DenyRead is unsupported on both
//     restricted-token runner levels under the narrow SID set (PR #640). The
//     fully restricted token cannot load ordinary system binaries without
//     Users/AuthUsers, and adding those groups reopens write grants outside
//     WriteRoots. Revisit once access-time confinement exists.
//   - A candidate nested under a user-configured AllowRead entry is dropped,
//     so `allowRead: ["~/.aws"]` remains an explicit opt-out. The bridge bearer
//     token is the one exception: the in-process tool boundary
//     (protectedCredentialPaths) treats it as non-overrideable, and the
//     guarantee must not depend on whether a wrapped shell command or a built-in
//     tool does the reading.
//   - Candidates are emitted whether or not they currently exist on disk.
//     Pathname-policy backends such as Seatbelt can enforce future paths;
//     mount-based Linux masks a path only if it exists when the namespace is
//     assembled, which is why directories derived from Zero's own process
//     environment are also reported as EnsureDirs (command-controlled roots
//     never are) and why third-party stores such as ~/.aws stay best-effort.
//   - Zero's own config directory is denied WHOLE, with the supported
//     non-secret subtrees carved back out. Only a directory-level rule covers
//     the temporary names its stores publish through and the files a concurrent
//     login creates mid-session; the carveouts keep the user plugin,
//     specialist, and command roots readable, since those are executed through
//     the sandbox.
//
// These are profile-level rules only; they are intentionally NOT merged into
// Policy.DenyRead, whose emptiness gates escalated (unsandboxed) execution and
// must keep reflecting user configuration alone.
func credentialDenyReadPaths(policy Policy, commandDir string, commandEnv []string, commandAllowedRoots []string) credentialDenyPaths {
	if runtime.GOOS == "windows" {
		return credentialDenyPaths{}
	}

	// Only roots derived from Zero's own process environment may produce
	// carveouts or host directories. CommandSpec.Env can contain project-controlled
	// MCP environment overrides, so it contributes deny-if-present paths only.
	processBaseDirs := credentialProcessBaseDirs()
	processOptions := credentialPathOptionsFromEnvironment(processBaseDirs, os.Environ())
	trusted := credentialDenyReadPathsIn(
		processOptions,
		policy.AllowRead,
	)
	// The daemon-token path is selected by this Zero process, not by a command's
	// supplied environment. Keep it as an exact mandatory subset so a command
	// cannot nominate an arbitrary host path for fail-closed materialization.
	trusted.MandatoryPaths = protectedCredentialPaths()
	trusted.Paths = dedupeStrings(append(trusted.Paths, trusted.MandatoryPaths...))
	processFinalFiles := credentialFinalTokenFiles(processOptions)
	allowRoots := normalizeProfilePaths(policy.AllowRead)
	for _, file := range processFinalFiles {
		// The token blob and encryption key are independently atomically replaced.
		// Preserve each terminal lexical name: rename replaces a terminal symlink
		// rather than following it, so allowing only its resolved target must not
		// expose the replacement pathname (nor does allowing the blob imply its key).
		for _, final := range []string{file, file + ".secret"} {
			candidate := normalizeCredentialFinalPath(final)
			if candidate == "" || credentialPathReincluded(allowRoots, candidate) {
				continue
			}
			trusted.Paths = append(trusted.Paths, candidate)
			trusted.ProcessTrustedFinalFiles = append(trusted.ProcessTrustedFinalFiles, candidate)
		}
	}
	trusted.Paths = dedupeStrings(trusted.Paths)
	trusted.ProcessTrustedFinalFiles = dedupeStrings(trusted.ProcessTrustedFinalFiles)
	processDirs := append([]string{}, trusted.Dirs...)
	appendUntrusted := func(options credentialPathOptions) {
		paths := credentialDenyReadPathsIn(options, policy.AllowRead)
		// A command-controlled credential setting cannot revoke a root that was
		// deliberately granted to that same command. Dropping only overlapping
		// command candidates preserves all unrelated process and command denies.
		paths.Paths = pathsOutsideOverlappingRoots(paths.Paths, commandAllowedRoots)
		paths.Dirs = pathsOutsideOverlappingRoots(paths.Dirs, commandAllowedRoots)
		trusted.Paths = append(trusted.Paths, paths.Paths...)
		trusted.Dirs = append(trusted.Dirs, paths.Dirs...)
		trusted.CommandDirs = append(trusted.CommandDirs, paths.Dirs...)
		// Carry the final token pathnames too. They are still untrusted — no
		// carveouts, no EnsureDirs, no host mutation — but a deny that a rename
		// can detach is not a deny, and skipping an absent one leaves whatever
		// the run publishes there readable. Recording them lets a backend that
		// cannot hold them durably fail closed instead, exactly as it already
		// does for the process-trusted stores.
		for _, file := range credentialFinalTokenFiles(options) {
			for _, final := range []string{file, file + ".secret"} {
				candidate := normalizeCredentialFinalPath(final)
				if candidate == "" || credentialPathReincluded(allowRoots, candidate) || len(pathsOutsideOverlappingRoots([]string{candidate}, commandAllowedRoots)) == 0 {
					continue
				}
				trusted.Paths = append(trusted.Paths, candidate)
				trusted.CommandFinalFiles = append(trusted.CommandFinalFiles, candidate)
			}
		}
	}
	commandBaseDirs := credentialCommandBaseDirs(commandDir)
	if len(commandBaseDirs) > 0 {
		// A nested Zero inherits the process environment but resolves relative
		// values from the sandboxed command's cwd.
		appendUntrusted(credentialPathOptionsFromEnvironment(commandBaseDirs, os.Environ()))
	}
	if commandEnv != nil {
		// A supplied environment may also be consumed by code in the running Zero
		// process before exec, so conservatively deny both process- and
		// command-relative resolutions. These remain untrusted: neither resolution
		// contributes carveouts or host directory creation.
		baseDirs := dedupeStrings(append(append([]string{}, processBaseDirs...), commandBaseDirs...))
		appendUntrusted(credentialPathOptionsFromEnvironment(baseDirs, commandEnv))
	}
	trusted.Paths = dedupeStrings(trusted.Paths)
	trusted.Dirs = dedupeStrings(trusted.Dirs)
	trusted.CommandDirs = pathsExcluding(dedupeStrings(trusted.CommandDirs), processDirs)
	return trusted
}

// zeroConfigReadCarveoutNames are the supported non-secret subtrees of
// <configDir>/zero. Their contents are extension code and prompts that a
// sandboxed command legitimately executes or reads (a user plugin's tool
// command lives below the plugin root), so the credential deny must not hide
// them. Nothing here holds a secret: credentials, tokens, and config live in
// files directly under <configDir>/zero, not in these subtrees.
var zeroConfigReadCarveoutNames = []string{"plugins", "specialists", "commands"}

// processCredentialBaseDir is the working directory this process started in,
// captured once during package initialization.
//
// It must not be re-read per call. The token stores resolve a relative override
// such as ZERO_OAUTH_TOKENS_PATH with filepath.Abs when the store is opened and
// then write to that fixed path for the rest of the run, so a deny rule computed
// from a later os.Getwd() would name a different file than the one being
// written — denying a path nothing uses while the real store stayed readable.
// Nothing in Zero calls os.Chdir today, which is what keeps this equal to the
// writers' resolution; pinning it means that stays true if one is ever added,
// instead of the two drifting apart per BuildCommandPlan.
var processCredentialBaseDir = func() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Clean(cwd)
}()

// credentialProcessBaseDirs returns the directory the running Zero process uses
// to resolve relative credential overrides.
func credentialProcessBaseDirs() []string {
	if strings.TrimSpace(processCredentialBaseDir) == "" {
		return nil
	}
	return []string{processCredentialBaseDir}
}

// credentialCommandBaseDirs returns the directory a sandboxed child (including
// a nested Zero) uses to resolve inherited relative credential overrides.
func credentialCommandBaseDirs(commandDir string) []string {
	if dir := strings.TrimSpace(commandDir); dir != "" {
		return []string{filepath.Clean(dir)}
	}
	return nil
}

func credentialPathOptionsFromEnvironment(baseDirs []string, env []string) credentialPathOptions {
	homes := resolveCredentialOverridePaths(credentialEnvValue(env, "HOME"), baseDirs)
	if len(homes) == 0 {
		homes = resolveCredentialOverridePaths(credentialEnvValue(env, "USERPROFILE"), baseDirs)
	}
	if len(homes) == 0 {
		home, err := os.UserHomeDir()
		if err == nil {
			homes = resolveCredentialOverridePaths(home, baseDirs)
		}
	}
	configDirs := resolveCredentialOverridePaths(credentialEnvValue(env, "XDG_CONFIG_HOME"), baseDirs)
	if len(configDirs) == 0 {
		for _, home := range homes {
			configDirs = append(configDirs, filepath.Join(home, ".config"))
		}
	}
	cloudSDKConfigDirs := resolveCredentialOverridePaths(credentialEnvValue(env, "CLOUDSDK_CONFIG"), baseDirs)
	if len(cloudSDKConfigDirs) == 0 {
		for _, configDir := range configDirs {
			cloudSDKConfigDirs = append(cloudSDKConfigDirs, filepath.Join(configDir, "gcloud"))
		}
		// gcloud's default remains ~/.config/gcloud even when XDG_CONFIG_HOME is
		// set. Keep both roots so the deny follows the tool as well as the
		// preserved upstream config-home baseline.
		for _, home := range homes {
			cloudSDKConfigDirs = append(cloudSDKConfigDirs, filepath.Join(home, ".config", "gcloud"))
		}
	}
	npmUserConfigs := resolveCredentialOverridePaths(firstNonEmpty(
		credentialEnvValue(env, "NPM_CONFIG_USERCONFIG"),
		credentialEnvValue(env, "npm_config_userconfig"),
	), baseDirs)
	if len(npmUserConfigs) == 0 {
		for _, home := range homes {
			npmUserConfigs = append(npmUserConfigs, filepath.Join(home, ".npmrc"))
		}
	}
	ghConfigDirs := resolveCredentialOverridePaths(credentialEnvValue(env, "GH_CONFIG_DIR"), baseDirs)
	if len(ghConfigDirs) == 0 {
		for _, configDir := range configDirs {
			ghConfigDirs = append(ghConfigDirs, filepath.Join(configDir, "gh"))
		}
	}
	netrcs := resolveCredentialOverridePaths(credentialEnvValue(env, "NETRC"), baseDirs)
	if len(netrcs) == 0 {
		for _, home := range homes {
			netrcs = append(netrcs, filepath.Join(home, ".netrc"))
		}
	}
	dockerConfigDirs := resolveCredentialOverridePaths(credentialEnvValue(env, "DOCKER_CONFIG"), baseDirs)
	if len(dockerConfigDirs) == 0 {
		for _, home := range homes {
			dockerConfigDirs = append(dockerConfigDirs, filepath.Join(home, ".docker"))
		}
	}
	var kubeConfigs []string
	if kubeConfig := strings.TrimSpace(credentialEnvValue(env, "KUBECONFIG")); kubeConfig != "" {
		for _, entry := range filepath.SplitList(kubeConfig) {
			kubeConfigs = append(kubeConfigs, resolveCredentialOverridePaths(entry, baseDirs)...)
		}
	} else {
		for _, home := range homes {
			kubeConfigs = append(kubeConfigs, filepath.Join(home, ".kube", "config"))
		}
	}
	return credentialPathOptions{
		Homes:              homes,
		ConfigDirs:         dedupeStrings(configDirs),
		CloudSDKConfigDirs: dedupeStrings(cloudSDKConfigDirs),
		GoogleCredentials:  resolveCredentialOverridePaths(credentialEnvValue(env, "GOOGLE_APPLICATION_CREDENTIALS"), baseDirs),
		NPMUserConfigs:     dedupeStrings(npmUserConfigs),
		GHConfigDirs:       dedupeStrings(ghConfigDirs),
		Netrcs:             dedupeStrings(netrcs),
		DockerConfigDirs:   dedupeStrings(dockerConfigDirs),
		KubeConfigs:        dedupeStrings(kubeConfigs),
		OAuthTokens:        resolveCredentialOverridePaths(credentialEnvValue(env, "ZERO_OAUTH_TOKENS_PATH"), baseDirs),
		OAuthStorage:       strings.TrimSpace(credentialEnvValue(env, "ZERO_OAUTH_STORAGE")),
		MCPOAuthTokens:     resolveCredentialOverridePaths(credentialEnvValue(env, "ZERO_MCP_OAUTH_TOKENS_PATH"), baseDirs),
	}
}

func credentialEnvValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		name, candidate, ok := strings.Cut(entry, "=")
		if ok && name == key {
			value = candidate
		}
	}
	return value
}

type credentialPathOptions struct {
	Homes              []string
	ConfigDirs         []string
	CloudSDKConfigDirs []string
	GoogleCredentials  []string
	NPMUserConfigs     []string
	GHConfigDirs       []string
	Netrcs             []string
	DockerConfigDirs   []string
	KubeConfigs        []string
	OAuthTokens        []string
	OAuthStorage       string
	MCPOAuthTokens     []string
}

// credentialFinalTokenFiles returns token pathnames whose selected backend
// atomically replaces files on disk. ZERO_OAUTH_TOKENS_PATH still contributes
// to the ordinary deny baseline when keyring storage is selected, but it must
// not make bubblewrap fail closed: the keyring backend never publishes the
// token blob or its encryption-key sibling at that path. The MCP override is a
// legacy migration input, not an output; the unified store writes through the
// OAuth path instead.
func credentialFinalTokenFiles(options credentialPathOptions) []string {
	if options.OAuthStorage == "keyring" {
		return nil
	}
	return append([]string{}, options.OAuthTokens...)
}

// credentialDenyReadPathsIn is the pure core of credentialDenyReadPaths,
// separated so tests can exercise it against a synthetic home directory.
func credentialDenyReadPathsIn(options credentialPathOptions, allowRead []string) credentialDenyPaths {
	var candidates []string
	var carveouts []string
	var ensureDirs []string
	var dirs []string
	for _, home := range options.Homes {
		if strings.TrimSpace(home) == "" {
			continue
		}
		homeDirs := []string{
			filepath.Join(home, ".aws"),
			filepath.Join(home, ".azure"),
		}
		candidates = append(candidates, homeDirs...)
		dirs = append(dirs, homeDirs...)
		// git's credential store backend, which holds host passwords and
		// personal access tokens in cleartext. Denied rather than the whole
		// of ~/.ssh, because these cost nothing functionally: git reads them
		// through a credential helper for authentication, not for identity,
		// so a sandboxed git still works and simply cannot authenticate as
		// the user. SSH key material is a harder trade and is tracked
		// separately (#815). A file, so it joins candidates only — dirs
		// drives directory-shaped handling (bwrap binds, carveouts).
		candidates = append(candidates, filepath.Join(home, ".git-credentials"))
	}
	candidates = append(candidates, options.GoogleCredentials...)
	candidates = append(candidates, options.NPMUserConfigs...)
	candidates = append(candidates, options.Netrcs...)
	candidates = append(candidates, options.KubeConfigs...)
	for _, dockerConfigDir := range options.DockerConfigDirs {
		if strings.TrimSpace(dockerConfigDir) != "" {
			candidates = append(candidates, filepath.Join(dockerConfigDir, "config.json"))
		}
	}
	for _, ghConfigDir := range options.GHConfigDirs {
		if strings.TrimSpace(ghConfigDir) != "" {
			candidates = append(candidates, filepath.Join(ghConfigDir, "hosts.yml"))
		}
	}
	for _, cloudSDKConfigDir := range options.CloudSDKConfigDirs {
		if strings.TrimSpace(cloudSDKConfigDir) == "" {
			continue
		}
		candidates = append(candidates, cloudSDKConfigDir)
		dirs = append(dirs, cloudSDKConfigDir)
	}
	for _, configDir := range options.ConfigDirs {
		if strings.TrimSpace(configDir) == "" {
			continue
		}
		// The XDG location of the same git credential store. Denying the file
		// rather than the directory is deliberate: ~/.config/git also holds
		// the global git config, which userGitConfigReadPaths grants on
		// purpose so a sandboxed git can read user.name and aliases instead
		// of failing outright. Added before the zero-directory handling below
		// so a normalization failure there cannot drop this deny with it.
		candidates = append(candidates, filepath.Join(configDir, "git", "credentials"))
		// Deny the whole directory rather than an itemized file list: Zero's
		// credential, token, and config stores publish through temporary siblings
		// before an atomic rename, the legacy MCP store leaves a
		// mcp-oauth-tokens.json.migrated backup behind, and a concurrent login can
		// add a store that did not exist when this profile was built. Only a
		// directory rule covers all three. Zero owns this directory, so it is also
		// an EnsureDir: bubblewrap cannot mask a path that is absent when the
		// namespace is assembled.
		// Normalize the denied root first, then derive its fixed children
		// lexically. This keeps nonexistent carveouts under the same canonical
		// parent (for example macOS /var -> /private/var) without following a
		// plugins/specialists/commands symlink into a credential file.
		zeroDir := normalizeProfilePath(filepath.Join(configDir, "zero"))
		if zeroDir == "" {
			continue
		}
		candidates = append(candidates, zeroDir)
		dirs = append(dirs, zeroDir)
		ensureDirs = append(ensureDirs, zeroDir)
		for _, name := range zeroConfigReadCarveoutNames {
			carveouts = append(carveouts, filepath.Join(zeroDir, name))
		}
	}
	for _, tokenPath := range options.OAuthTokens {
		if options.OAuthStorage == "keyring" {
			// The keyring backend does not open the override or any publication
			// siblings. Keep the pathname as a conservative ordinary deny, but do
			// not manufacture unused .publish directories on the host.
			candidates = append(candidates, tokenPath)
		} else {
			candidates = append(candidates, credentialTokenStorePaths(tokenPath)...)
			publicationDirs := credentialPublicationDirs(tokenPath)
			dirs = append(dirs, publicationDirs...)
			ensureDirs = append(ensureDirs, publicationDirs...)
		}
	}
	for _, tokenPath := range options.MCPOAuthTokens {
		// This is a legacy migration input. MCP reads it and renames it aside;
		// normal writes go to the separately resolved unified OAuth store.
		// Protect both token-bearing names, but do not classify the source as an
		// atomic writer or create publication directories it never uses.
		candidates = append(candidates, tokenPath, tokenPath+".migrated")
	}
	allowRoots := normalizeProfilePaths(allowRead)
	out := make([]string, 0, len(candidates))
	for _, path := range normalizeProfilePaths(candidates) {
		if credentialPathReincluded(allowRoots, path) {
			continue
		}
		out = append(out, path)
	}
	return credentialDenyPaths{
		Paths:      dedupeStrings(out),
		Carveouts:  credentialCarveoutPaths(out, carveouts),
		EnsureDirs: credentialRetainedDirs(out, normalizeProfilePaths(ensureDirs)),
		Dirs:       credentialRetainedDirs(out, normalizeProfilePaths(dirs)),
	}
}

// credentialTokenStorePaths returns the deny entries for one token-store path:
// the store, its lock siblings, its encryption-key sibling, and the directory
// it publishes new contents through. The names are fixed so an override outside
// Zero's config directory is protected by exact rules instead of hiding an
// arbitrary parent such as the workspace or /tmp.
func credentialTokenStorePaths(tokenPath string) []string {
	if strings.TrimSpace(tokenPath) == "" {
		return nil
	}
	paths := []string{
		tokenPath,
		tokenPath + ".lockfile",
		tokenPath + ".secret",
		tokenPath + ".secret.lock",
		// Left behind by a Zero older than the publication directories below.
		tokenPath + ".tmp",
		tokenPath + ".secret.tmp",
	}
	return append(paths, credentialPublicationDirs(tokenPath)...)
}

// credentialPublicationDirs are the per-store directories the OAuth stores
// create their randomly-named temporary file in (see oauth.PublicationDir) —
// one for the token blob, one for its encryption key. The directory NAME is
// derived from the store path so the profile can deny it up front, while the
// random name inside it is what keeps a sandboxed same-user process from
// opening, or renaming away, the file that briefly holds the plaintext.
func credentialPublicationDirs(tokenPath string) []string {
	if strings.TrimSpace(tokenPath) == "" {
		return nil
	}
	return []string{
		tokenPath + credentialPublicationDirSuffix,
		tokenPath + ".secret" + credentialPublicationDirSuffix,
	}
}

// credentialPublicationDirSuffix mirrors oauth.PublicationDirSuffix, duplicated
// because internal/mcp depends on this package and internal/oauth must stay
// importable from both.
const credentialPublicationDirSuffix = ".publish"

// pathsOutsideRoots drops every path that lies within one of roots.
func pathsOutsideRoots(paths []string, roots []string) []string {
	if len(paths) == 0 || len(roots) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if credentialPathReincluded(roots, path) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// pathsOutsideOverlappingRoots drops paths that contain, or are contained by, a
// root. A credential carveout is an allow-back rule, so either overlap could
// weaken a user deny after Seatbelt's last-match-wins evaluation.
func pathsOutsideOverlappingRoots(paths []string, roots []string) []string {
	if len(paths) == 0 || len(roots) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		overlaps := false
		for _, root := range roots {
			if pathWithinRoot(root, path) || pathWithinRoot(path, root) {
				overlaps = true
				break
			}
		}
		if !overlaps {
			out = append(out, path)
		}
	}
	return out
}

func credentialPathReincluded(allowRoots []string, path string) bool {
	for _, allow := range allowRoots {
		if pathWithinRoot(allow, path) {
			return true
		}
	}
	return false
}

// credentialCarveoutPaths keeps only the carveouts that sit inside a path that
// is actually denied, so an AllowRead opt-out that removed the deny does not
// leave a stray allow-back rule behind.
func credentialCarveoutPaths(denied []string, carveouts []string) []string {
	if len(carveouts) == 0 {
		return nil
	}
	denied = normalizeProfilePaths(denied)
	out := make([]string, 0, len(carveouts))
	for _, entry := range carveouts {
		carveout := normalizeCredentialCarveoutPath(entry)
		if carveout == "" {
			continue
		}
		for _, deny := range denied {
			if carveout != deny && pathWithinRoot(deny, carveout) {
				out = append(out, carveout)
				break
			}
		}
	}
	return dedupeStrings(out)
}

// normalizeCredentialCarveoutPath canonicalizes the parent while preserving
// the fixed terminal name. That keeps the allow under the same canonical root
// as its deny (for example macOS /var -> /private/var) without ever following a
// plugins/specialists/commands symlink to a credential target.
func normalizeCredentialCarveoutPath(entry string) string {
	carveout := normalizeProfilePathLexically(entry)
	if carveout == "" {
		return ""
	}
	// A missing fixed subtree may be installed later by trusted host code, but
	// an existing entry must be a real directory. In particular, never turn a
	// plugins symlink into an allow rule for its credential-file target.
	if info, err := os.Lstat(carveout); err == nil {
		if !info.IsDir() {
			return ""
		}
	} else if !os.IsNotExist(err) {
		return ""
	}
	parent := normalizeProfilePath(filepath.Dir(carveout))
	if parent == "" {
		return ""
	}
	return filepath.Join(parent, filepath.Base(carveout))
}

// credentialRetainedDirs keeps only directories that remain exact deny entries.
func credentialRetainedDirs(denied []string, dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		for _, deny := range denied {
			if dir == deny {
				out = append(out, dir)
				break
			}
		}
	}
	return dedupeStrings(out)
}

// finalizeCredentialDenyPaths gives user denies precedence and removes nested
// automatic mounts that an outer automatic directory mask already covers.
// Children inside a retained carveout stay explicit denies, because the
// carveout re-allows that subtree.
func finalizeCredentialDenyPaths(credentials credentialDenyPaths, userDenyRead []string) credentialDenyPaths {
	// A broader user read denial may subsume optional credential stores, but the
	// selected daemon token remains exact and mandatory: native backends also use
	// this entry to deny writes, which a parent read denial does not imply.
	credentials.MandatoryPaths = dedupeStrings(credentials.MandatoryPaths)
	credentials.Paths = dedupeStrings(append(pathsOutsideRoots(credentials.Paths, userDenyRead), credentials.MandatoryPaths...))
	credentials.Carveouts = pathsOutsideOverlappingRoots(credentials.Carveouts, userDenyRead)
	credentials.Dirs = credentialRetainedDirs(credentials.Paths, credentials.Dirs)
	credentials.Carveouts = credentialCarveoutPaths(credentials.Paths, credentials.Carveouts)

	mandatory := make(map[string]struct{}, len(credentials.MandatoryPaths))
	for _, path := range credentials.MandatoryPaths {
		mandatory[path] = struct{}{}
	}
	var paths []string
	for _, path := range credentials.Paths {
		// An optional automatic directory is not durable coverage for a mandatory
		// child: Linux may skip that directory when it is absent. Keep the exact
		// daemon-token pathname so the backend can enforce or reject it separately.
		if _, ok := mandatory[path]; ok {
			paths = append(paths, path)
			continue
		}
		covered := false
		for _, dir := range credentials.Dirs {
			if path != dir && pathWithinRoot(dir, path) && !credentialPathReincluded(credentials.Carveouts, path) {
				covered = true
				break
			}
		}
		if !covered {
			paths = append(paths, path)
		}
	}
	credentials.Paths = dedupeStrings(paths)
	retainedPaths := make(map[string]struct{}, len(credentials.Paths))
	for _, path := range credentials.Paths {
		retainedPaths[path] = struct{}{}
	}
	var retainedMandatory []string
	for _, path := range credentials.MandatoryPaths {
		if _, ok := retainedPaths[path]; ok {
			retainedMandatory = append(retainedMandatory, path)
		}
	}
	credentials.MandatoryPaths = dedupeStrings(retainedMandatory)
	credentials.Dirs = credentialRetainedDirs(credentials.Paths, credentials.Dirs)
	credentials.CommandDirs = credentialRetainedDirs(credentials.Paths, credentials.CommandDirs)
	credentials.Carveouts = credentialCarveoutPaths(credentials.Paths, credentials.Carveouts)
	credentials.EnsureDirs = credentialRetainedDirs(credentials.Paths, credentials.EnsureDirs)
	credentials.ProcessTrustedFinalFiles = credentialRetainedFiles(
		credentials.ProcessTrustedFinalFiles,
		userDenyRead,
		credentials.EnsureDirs,
		credentials.Carveouts,
	)
	// Same retention for the command-derived finals: a file already inside a
	// user deny or a directory Zero will actually mask is durably covered, so it
	// is not a fail-closed reason. What survives is an override pointing OUTSIDE
	// every masked directory — the case a bubblewrap bind cannot hold.
	credentials.CommandFinalFiles = credentialRetainedFiles(
		credentials.CommandFinalFiles,
		userDenyRead,
		credentials.EnsureDirs,
		credentials.Carveouts,
	)
	// A command environment is also resolved against the process base dir, so an
	// absolute override in Zero's own environment lands in both lists. Report it
	// once, as the process-trusted store it is.
	credentials.CommandFinalFiles = pathsExcluding(credentials.CommandFinalFiles, credentials.ProcessTrustedFinalFiles)
	return credentials
}

// pathsExcluding returns paths with every exact member of excluded removed.
func pathsExcluding(paths, excluded []string) []string {
	if len(paths) == 0 || len(excluded) == 0 {
		return paths
	}
	skip := make(map[string]struct{}, len(excluded))
	for _, path := range excluded {
		skip[path] = struct{}{}
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, dup := skip[path]; dup {
			continue
		}
		out = append(out, path)
	}
	return out
}

// credentialRetainedFiles drops fail-closed file markers only when a durable
// directory mask strictly contains the file. Exact file mounts are vulnerable
// to atomic replacement, and Dirs may include command-controlled candidates;
// only user directory ancestors and retained trusted EnsureDirs qualify. A
// trusted directory does not cover a final file that is re-exposed through a
// readable carveout below it.
func credentialRetainedFiles(files, userDenied, trustedDeniedDirs, carveouts []string) []string {
	var out []string
	for _, file := range files {
		covered := false
		for _, dir := range userDenied {
			if file != dir && pathWithinRoot(dir, file) {
				covered = true
				break
			}
		}
		if !covered {
			for _, dir := range trustedDeniedDirs {
				if file != dir && pathWithinRoot(dir, file) && !credentialFileReincludedByCarveout(dir, file, carveouts) {
					covered = true
					break
				}
			}
		}
		if !covered {
			out = append(out, file)
		}
	}
	return dedupeStrings(out)
}

func credentialFileReincludedByCarveout(deniedDir, file string, carveouts []string) bool {
	for _, carveout := range carveouts {
		if pathWithinRoot(deniedDir, carveout) && pathWithinRoot(carveout, file) {
			return true
		}
	}
	return false
}

// resolveCredentialOverridePaths mirrors the token stores' own override
// resolution (oauth.ResolveStorePath, mcp.ResolveTokenStorePath — reimplemented
// here rather than imported because internal/mcp depends on this package): the
// value is used literally, NOT tilde-expanded the way normalizeProfilePath
// expands other candidates. Using normalizeProfilePath here would derive a deny
// path that doesn't match where the store actually writes — e.g.
// ZERO_OAUTH_TOKENS_PATH=~/x resolves to <cwd>/~/x on disk (the store never
// expands "~"), but normalizeProfilePath would deny $HOME/x instead, leaving
// the real file unprotected.
//
// A relative value yields one candidate per supplied base dir because the
// process that resolves it is not necessarily the one that writes the store.
func resolveCredentialOverridePaths(override string, baseDirs []string) []string {
	override = strings.TrimSpace(override)
	if override == "" {
		return nil
	}
	if filepath.IsAbs(override) {
		return []string{filepath.Clean(override)}
	}
	out := make([]string, 0, len(baseDirs))
	for _, baseDir := range baseDirs {
		if strings.TrimSpace(baseDir) == "" {
			continue
		}
		out = append(out, filepath.Clean(filepath.Join(baseDir, override)))
	}
	return dedupeStrings(out)
}

// userGitConfigReadPaths returns the user's global git config FILES so a
// sandboxed git can read identity and config (user.name/email, aliases) instead
// of failing with "unable to access ~/.gitconfig". On macOS, where reads are
// allow-listed, granting only these files avoids exposing the surrounding
// configuration directory. Linux uses a read-all profile with explicit
// credential deny rules. The paths are granted at the macOS seatbelt read rule
// rather than the cross-platform PermissionProfile so HOME-dependent paths
// don't leak into the platform-agnostic policy snapshot.
func userGitConfigReadPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".config", "git", "config"),
	}
}

func profileRootPath() string {
	return filepath.Clean(string(filepath.Separator))
}

func normalizeProfileDirs(entries []string) []string {
	paths := normalizeProfilePaths(entries)
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.IsDir() && filepath.Dir(path) != path {
			out = append(out, path)
		}
	}
	return out
}

func normalizeProfilePaths(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		path := normalizeProfilePath(entry)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

func normalizeProfilePath(entry string) string {
	absolute := normalizeProfilePathLexically(entry)
	if absolute == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}

	// EvalSymlinks requires the whole path to exist. Resolve the deepest existing
	// ancestor and append the missing tail lexically so future deny paths still
	// match canonical aliases such as macOS /var -> /private/var.
	current := absolute
	var tail []string
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute)
		}
		tail = append([]string{filepath.Base(current)}, tail...)
		current = parent
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
	}
}

// normalizeCredentialFinalPath canonicalizes only the parent of an absolute,
// already-resolved credential override and rejoins its terminal lexical name.
// normalizeProfilePath supplies deepest-existing-ancestor handling for a
// missing parent without following a terminal symlink that os.Rename replaces.
func normalizeCredentialFinalPath(path string) string {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return ""
	}
	parent := normalizeProfilePath(filepath.Dir(filepath.Clean(path)))
	if parent == "" {
		return ""
	}
	return filepath.Join(parent, filepath.Base(filepath.Clean(path)))
}

// normalizeProfilePathLexically expands and absolutizes a profile path without
// resolving symlinks. Credential carveouts use it so their fixed lexical name
// can never become an allow rule for a symlink target.
func normalizeProfilePathLexically(entry string) string {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return ""
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		trimmed = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(trimmed[1:], "/"), string(filepath.Separator)))
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}
