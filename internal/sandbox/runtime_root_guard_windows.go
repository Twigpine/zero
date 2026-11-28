//go:build windows

package sandbox

import (
	"os"
	"strings"
)

// refuseForeignRuntimeComponent has no ownership check on Windows.
//
// The derived root lives under the per-user cache directory or the per-session
// TEMP, both of which are already user-private, and the elevated setup path
// applies its own capability ACL. The link refusal in the shared guard is the
// part that matters here.
func refuseForeignRuntimeComponent(string, os.FileInfo) error {
	return nil
}

// sandboxRuntimeUserScope names the account the tree belongs to.
//
// Windows TEMP is already per-user, so this is belt and braces rather than the
// load-bearing separation it is on Unix. Kept so the derived path has the same
// shape on every platform and one code path builds it.
//
// FROM THE TOKEN, NOT THE ENVIRONMENT. Setup derives the fallback root in the
// elevated terminal and records it; a command later re-derives it in an
// ordinary one to decide whether that record belongs to this workspace. Those
// are two processes with independent environments, so a USERNAME that differed
// between them made the command reject the root setup had provisioned and
// select one that carries no capability ACE. The token's user SID is the same
// in both, because setup refuses to run as any account other than the one that
// will use Zero. The Unix build reads the uid for the same reason. Reported by
// CodeRabbit.
func sandboxRuntimeUserScope() string {
	sid, err := currentProcessSID()
	if err != nil {
		return "u"
	}
	return "u" + strings.ToLower(sid)
}

// sandboxRuntimeFallbackOwnedNames are the components the temp-derived runtime
// root is built from. Unscoped on Windows: os.TempDir() already resolves inside
// the user's own profile, so the shared-temp collision the Unix build guards
// against cannot arise, and the fixed names keep windowsSandboxRuntimeOwnedTail
// able to recognise a root built by an elevated setup running as another
// account.
func sandboxRuntimeFallbackOwnedNames() []string {
	return windowsSandboxRuntimeOwnedNames
}
