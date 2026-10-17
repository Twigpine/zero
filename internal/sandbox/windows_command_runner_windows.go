//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func runWindowsSandboxCommand(config WindowsSandboxCommandConfig, stderr io.Writer) int {
	// Fully restricted DenyRead tokens cannot load ordinary Users-granted
	// system binaries without SID broadening; broadening is permanently off
	// because it admits write grants outside WriteRoots. Reject on both the
	// elevated and unelevated restricted-token tiers before setup or launch
	// until access-time confinement exists (PR #640).
	if err := windowsDenyReadRestrictedTokenUnsupported(config); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	switch config.SandboxLevel {
	case WindowsSandboxLevelRestrictedToken:
		if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(config)); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
		// The marker attests to a PLAN. This attests to the OBJECT that plan was
		// applied to, which cleanup can reclaim and an ordinary run then recreates
		// without the capability ACE. Checked before the token is minted, so the
		// operator is told to rerun setup instead of watching every sandboxed write
		// fail with a bare access-denied.
		if err := verifyWindowsRuntimeRootCapability(config); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
	case WindowsSandboxLevelUnelevated:
		if err := ensureWindowsUnelevatedSetup(config); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
	default:
		fmt.Fprintf(stderr, "%s: unsupported Windows sandbox level %q\n", WindowsSandboxCommandRunnerName, config.SandboxLevel)
		return 1
	}
	if err := ValidateWindowsNetworkPolicy(config.PermissionProfile.Network); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	capabilitySIDs, err := WindowsCapabilitySIDsForConfig(config)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	offlineSID, err := WindowsOfflineMarkerSID(config.SandboxHome)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	// Compose the restricting-SID set: both modes keep the write-capability SIDs
	// (workspace write-jail); deny additionally carries the offline-marker SID
	// that the persistent WFP block filter matches — so a deny command has no
	// network while an approved allow command reaches it, both write-jailed.
	//
	// KNOWN LIMITATION: an approved online command reaches the network, but HTTPS
	// via Windows Schannel (e.g. a Schannel-backed curl.exe) fails inside this
	// restricted token with SEC_E_NO_CREDENTIALS — Schannel can't acquire its
	// per-user TLS credential under a WRITE_RESTRICTED/LUA token. This is a
	// fundamental restricted-token vs Schannel incompatibility (the standard
	// mitigation is to run TLS in a broker process, not the sandboxed one) and
	// has no clean in-token fix. Workarounds: the degraded path (no restricted
	// token) or the in-process web_fetch tool.
	//
	// KNOWN LIMITATION: MSYS2/Cygwin binaries (Git for Windows bash.exe,
	// sh.exe, and the usr\bin coreutils) cannot initialize under this token at
	// all, whether invoked directly or spawned internally by an otherwise
	// native command (git hooks, git/gh credential helpers). The MSYS runtime
	// secures its signal pipe and shared-memory sections with explicit DACLs
	// granting only the user, Administrators, and SYSTEM (msys2-runtime
	// sigproc.cc sigproc_init -> sec_user_nih -> __sec_user), and a
	// WRITE_RESTRICTED write check must ALSO match one of the token's
	// restricted SIDs (logon SID, Everyone, capability SIDs). None of the
	// granted SIDs can be added to the restricted list without collapsing the
	// write jail (each has write access nearly everywhere), so MSYS startup
	// dies with "couldn't create signal pipe" or "CreateFileMapping <SID>.1",
	// Win32 error 5, and exit status 0xC0000142. The System32 WSL bash
	// launcher fails equivalently (the restricted token cannot connect to the
	// WSL service: Bash/Service/CreateInstance/E_ACCESSDENIED). Like Schannel,
	// this has no in-token fix; preflight blocking and output hints live in
	// internal/tools/shell_runtime.go.
	tokenSIDs := windowsRuntimeTokenSIDs(capabilitySIDs, offlineSID, config.PermissionProfile.Network.Mode)
	// WRITE_RESTRICTED keeps reads unrestricted so sandboxed commands can load
	// Users-granted executables. It is only used when DenyRead is empty (#612:
	// WRITE_RESTRICTED skips restricted-SID deny ACEs for reads). Non-empty
	// DenyRead is rejected above for both runner levels rather than launching a
	// fully restricted narrow-SID token that cannot execute normal tools.
	writeRestricted := len(config.PermissionProfile.FileSystem.DenyRead) == 0

	// A provisioned sandbox principal replaces the restricted token entirely: it
	// is a separate account, so reads outside its granted roots are denied by the
	// filesystem rather than left open the way a same-user restricted token has
	// to leave them (#662). Absent, unprovisioned or opted-out, ok is false and
	// the restricted-token backend below runs exactly as before.
	principalToken, ok, err := windowsSandboxPrincipalToken(config)
	if err != nil {
		// The one path here that does not fall back, because a provisioned but
		// unusable principal means the sandbox is broken rather than absent. Say
		// how to get out of it, since the whole backend is opt-in.
		fmt.Fprintf(stderr, "%s: sandbox principal is provisioned but unusable: %v. Re-run `zero sandbox setup` from an elevated terminal, or unset %s to fall back to the restricted-token sandbox.\n",
			WindowsSandboxCommandRunnerName, err, windowsSandboxIdentityEnv)
		return 1
	}
	if ok {
		defer principalToken.Close()
		// The principal gets its own identity AND the write jail, not one or the
		// other. Its ACEs confine reads; without the restricted token it would
		// still hold every write its ambient memberships grant, so a profile
		// permitting writes only to the workspace could still write anywhere
		// BATCH or BUILTIN\Users may — C:\Users\Public\Documents, for one.
		//
		// The principal's own SID is deliberately NOT a restricting SID, and that
		// is the whole of the write jail on this path.
		//
		// WRITE_RESTRICTED allows a write only when BOTH the normal token and the
		// restricting-SID list allow it. The account SID is already enabled in the
		// normal token, so listing it as a restricting SID makes the second check
		// a formality for anything granted to that account: every path carrying a
		// direct principal ACE passes both halves and is writable wherever it
		// sits. The principal's own profile directory, which Windows creates on
		// first logon with exactly such an ACE, is outside every configured write
		// root and was writable for that reason.
		//
		// Same defect as the World SID in the restricted list (#865), with the
		// account SID in place of Everyone: a SID already carried by the normal
		// token cannot also serve as the restriction on it.
		//
		// Confining to the capability SIDs alone restores the intersection the
		// jail is supposed to be. A configured write root carries BOTH a principal
		// ACE, from the principal plan, satisfying the normal token, AND a
		// capability ACE, from BuildWindowsACLPlan which setup applies on every
		// path, satisfying the restriction. So the principal keeps the tree it
		// owns, while anything holding only a principal ACE now fails the
		// restricted check.
		principalUser, err := principalToken.GetTokenUser()
		if err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": read sandbox principal SID: "+err.Error())
			return 1
		}
		jailSIDs := windowsPrincipalJailSIDs(tokenSIDs, principalUser.User.Sid.String())
		// A strict token restricts READS too, and the account SID that carries the
		// read roots was just removed above. Without the read capability the jail
		// stops being a write jail and becomes a total one: no read root, no
		// executable, nothing. BuildWindowsACLPlan grants this same SID on every
		// read root and denies it on every DenyRead path, so the allow-list and the
		// restriction stay one decision.
		if !writeRestricted {
			readSID, err := WindowsReadAllowSID(config.SandboxHome)
			if err != nil {
				fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": resolve sandbox read capability: "+err.Error())
				return 1
			}
			jailSIDs = append(jailSIDs, readSID)
		}
		jailedToken, err := restrictWindowsTokenForCapabilitySIDs(principalToken, jailSIDs, writeRestricted)
		if err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
		defer jailedToken.Close()
		// CreateProcessAsUser below is about to be handed a token for a DIFFERENT
		// account, which forfeits the own-token exemption the ordinary restricted
		// path relies on. Enable what that needs, and refuse with a specific reason
		// when this process cannot, rather than letting an unelevated run surface a
		// bare "Access is denied" that reads as the command being rejected.
		//
		// Refusing rather than falling back: an operator who set the opt-in to
		// confine reads must not be handed the same-user restricted token while
		// believing otherwise.
		if err := enableWindowsPrincipalLaunchPrivileges(); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
		// Stop the child describing the CALLER. Everything identifying the account
		// survived from the invoking user's environment, so a principal command
		// resolved its per-user state through paths inside a profile it cannot
		// open. Applied here, on the principal path only, because this is the first
		// point that knows which account the command is about to run as.
		// The role has to be the one the token above was minted for, or the child
		// is told it is the other account and resolves its per-user state under a
		// profile it cannot open.
		config.Env = windowsPrincipalIdentityEnvironment(
			config.Env,
			windowsSandboxUserName(
				windowsSandboxPrincipalKey(config),
				windowsSandboxRoleForNetwork(config.PermissionProfile.Network.Mode),
			),
			config.PermissionProfile.Runtime,
		)
		exitCode, err := runWindowsCommandAsUser(jailedToken, config)
		if err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
		return exitCode
	}

	token, err := createWindowsRestrictedTokenForCapabilitySIDs(tokenSIDs, writeRestricted)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	defer token.Close()
	exitCode, err := runWindowsCommandAsUser(token, config)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	return exitCode
}

// ensureWindowsUnelevatedSetup applies the workspace ACL plan from the current
// (non-elevated) process so the write-restricted token has somewhere its
// capability SIDs are granted. DACL edits on user-owned workspace and temp
// roots need no Administrator rights; the WFP network filters DO, so this tier
// provisions no network enforcement — the offline-marker SID composed into the
// token stays inert until an elevated `zero sandbox setup` installs the block
// filters. Applied plans are recorded by hash so repeat commands skip the
// re-apply; like the elevated setup, grants are left in place (the rollback is
// deliberately discarded) because they only name synthetic capability SIDs
// that no other token carries.
func ensureWindowsUnelevatedSetup(config WindowsSandboxCommandConfig) error {
	applied, plan, err := buildWindowsUnelevatedAppliedPlan(config)
	if err != nil {
		return err
	}
	marker, err := loadWindowsUnelevatedSetupMarker(config.SandboxHome)
	if err != nil {
		return err
	}
	// THE MARKER ATTESTS TO A PLAN. THE COMMAND CONSUMES AN OBJECT.
	//
	// The marker fingerprints pathnames and actions, and cleanup is allowed to
	// reclaim the deterministic runtime directory that plan was realized on. An
	// ordinary later run recreates the same pathname with the caller-private DACL
	// and no capability ACE, and the serialized plan is unchanged, so this fast
	// path returned before anything looked at the directory. The restricted child
	// still carried the capability SID; the new object simply did not grant it, and
	// every temp, package-cache and build-cache write failed after launch with a
	// bare ACCESS_DENIED and nothing pointing at setup.
	//
	// This tier owns its plan and needs no elevation, so the answer here is to
	// reapply rather than to refuse. Refusing would print advice to run elevated
	// setup, which is unnecessary on this tier and unfollowable for a user with no
	// Administrator account. The restricted-token tier keeps its refusal, because
	// an ordinary user cannot restore principal provisioning.
	if marker.contains(applied) && verifyWindowsRuntimeRootCapability(config) == nil {
		return nil
	}
	// THE TIER BOUNDARY, STATED BEFORE ANY MUTATION IS ATTEMPTED.
	//
	// One plan is consumed by two tiers with different authority. A profile that
	// carries DenyRead runs on a strict token, and the strict token applies the
	// restricted-SID check to READS, so the read capability has to be granted
	// wherever the command reads from -- including the volume root that
	// permissionProfileReadRoots seeds. Elevated setup can write that DACL. An
	// ordinary user cannot, and the common opener asks for WRITE_DAC on every
	// entry, so this tier fails on that one root every time.
	//
	// Dropping the root ACE instead is not an option: the strict token would then
	// fail its own read check for the executable and every ambient read. So the
	// tier is refused explicitly, naming the root and the reason, rather than
	// discovered as an ACCESS_DENIED after the fact. The real smoke test misses
	// this because it substitutes a user-owned temporary directory for the
	// production read root.
	if refusal := WindowsACLPlanReadGrantRefusal(plan); refusal != "" {
		return errors.New("unelevated sandbox setup cannot apply this plan: " + refusal)
	}
	if _, err := applyWindowsACLPlan(plan); err != nil {
		// Refusing to run is right: without these ACEs the write jail does not
		// exist, so continuing would run the command believing it is sandboxed
		// when it is not. What was wrong was the diagnosis. Every failure got the
		// same "the workspace may be on a filesystem you do not own" guess, and
		// the suggested remedy was elevated setup, which does not help at all
		// when the real problem is one root in the plan that nobody can ACL.
		//
		// Being precise matters because this failure repeats: the success marker
		// is only recorded on success, so the same plan fails identically on
		// every later command until the offending root leaves it. A reader who
		// cannot tell which root is at fault has no way out of that.
		// Every remedy named below is one the reader can actually carry out.
		// Both messages used to offer `--sandbox forbid`, which is not an option
		// at all: SandboxPreferenceForbid is an internal engine state with no flag
		// behind it, so acting on it produced an unknown option and left the reader
		// stuck on the failure they had just been told how to clear. Advice that
		// does not work costs more than none, since finding that out takes time.
		if denied := windowsACLPlanDeniedPath(err); denied != "" {
			return fmt.Errorf("apply unelevated workspace ACLs: %w; %s cannot have its permissions changed by this user, "+
				"so the sandbox cannot enforce a write boundary there and will not run the command. "+
				"That path is one of this workspace's sandbox roots, usually a system directory that arrived via TEMP or TMP. "+
				"Check those, or turn the sandbox off in your user config with "+
				`"sandbox": {"enabled": false}. `+
				"Running `zero sandbox setup` elevated will NOT fix this", err, denied)
		}
		return fmt.Errorf("apply unelevated workspace ACLs: %w — the workspace may be on a filesystem the current user does not own; "+
			"run `zero sandbox setup` from an elevated (Administrator) terminal, "+
			`or turn the sandbox off in your user config with "sandbox": {"enabled": false}`, err)
	}
	return recordWindowsUnelevatedAppliedPlan(config.SandboxHome, applied)
}

// windowsACLPlanDeniedPath pulls the target path out of an apply failure that
// was an access denial, and returns "" for anything else.
//
// applyWindowsACLPathGroup already wraps the path into its error, so this reads
// the message rather than threading a typed error through four layers for one
// diagnostic. The string it matches is produced in the same package by
// openWindowsACLTarget, and a test pins the pairing so the two cannot drift
// apart silently.
func windowsACLPlanDeniedPath(err error) string {
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return ""
	}
	const marker = "open windows ACL target "
	message := err.Error()
	start := strings.Index(message, marker)
	if start < 0 {
		return ""
	}
	// Colon-SPACE, not colon. The wrapper is "...target %s: %w", and on Windows
	// the path itself starts with a drive colon, so splitting on the first colon
	// returns "C". A drive colon is always followed by a separator, never a
	// space, which makes ": " the only unambiguous boundary here.
	rest := message[start+len(marker):]
	end := strings.Index(rest, ": ")
	if end <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// windowsPrincipalJailSIDs returns the restricting SIDs for a principal command.
//
// A named function with its own test rather than an inline slice literal,
// because the defect it encodes was invisible: the production composition added
// the account's own SID while the only jail test granted a GROUP, so the
// configuration that shipped was never the configuration under test.
//
// accountSID is taken and DELIBERATELY EXCLUDED rather than simply not passed.
// Naming it here makes the exclusion the function's stated contract instead of an
// omission a later edit could undo without noticing, and it also removes the SID
// should it ever arrive through capabilitySIDs, which is the same route the World
// SID took in #865.
func windowsPrincipalJailSIDs(capabilitySIDs []string, accountSID string) []string {
	account := strings.TrimSpace(accountSID)
	jail := make([]string, 0, len(capabilitySIDs))
	for _, sid := range capabilitySIDs {
		if account != "" && strings.EqualFold(strings.TrimSpace(sid), account) {
			continue
		}
		jail = append(jail, sid)
	}
	return jail
}
