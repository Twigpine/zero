package sandbox

import (
	"fmt"
	"strings"
)

// legacyFullAutoPermissionMode is what full-auto used to be called on disk.
//
// Accepted permanently rather than migrated: this value lives in user configs
// and in scripts, and a session that silently fell back to auto because its
// saved mode no longer parsed would be a confusing downgrade rather than a
// visible error.
const legacyFullAutoPermissionMode PermissionMode = "unsafe"

func NormalizePermissionMode(value PermissionMode) PermissionMode {
	normalized := PermissionMode(strings.ToLower(strings.TrimSpace(string(value))))
	switch normalized {
	case PermissionModeAsk:
		return PermissionModeAsk
	case PermissionFullAuto, legacyFullAutoPermissionMode:
		return PermissionFullAuto
	default:
		return PermissionModeAuto
	}
}

func NormalizePermission(value Permission) Permission {
	normalized := Permission(strings.ToLower(strings.TrimSpace(string(value))))
	switch normalized {
	case PermissionAllow, PermissionDeny:
		return normalized
	default:
		return PermissionPrompt
	}
}

func NormalizeSideEffect(value SideEffect) SideEffect {
	normalized := SideEffect(strings.ToLower(strings.TrimSpace(string(value))))
	switch normalized {
	case SideEffectRead, SideEffectWrite, SideEffectShell, SideEffectNetwork, SideEffectLocalControl, SideEffectLocalBrowser, SideEffectLocalDesktop, SideEffectLocalTerminal, SideEffectOutOfWorkspace, SideEffectNone:
		return normalized
	default:
		return SideEffectOutOfWorkspace
	}
}

func NormalizeNetworkMode(value NetworkMode) NetworkMode {
	normalized := NetworkMode(strings.ToLower(strings.TrimSpace(string(value))))
	if normalized == NetworkAllow {
		return NetworkAllow
	}
	return NetworkDeny
}

func NormalizeGrantDecision(value GrantDecision) (GrantDecision, error) {
	normalized := GrantDecision(strings.ToLower(strings.TrimSpace(string(value))))
	switch normalized {
	case GrantAllow, GrantDeny:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid sandbox grant decision %q. Expected allow or deny", value)
	}
}
