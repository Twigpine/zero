//go:build windows

package sandbox

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// currentProcessSID is the SID of the token running this process.
//
// Called from BuildWindowsSandboxSetupArgs, which runs in the OPERATOR'S shell
// before elevation, so it answers "who will read the stamp afterwards" rather
// than "who is provisioning it". Those are different principals whenever setup
// elevates, and the difference is the whole point: the elevated helper creates
// the runtime leaf when it is absent, so anything inferred from that leaf
// describes the installer and not the consumer.
func currentProcessSID() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolve the calling user SID for sandbox setup: %w", err)
	}
	sid := user.User.Sid
	if sid == nil {
		return "", fmt.Errorf("resolve the calling user SID for sandbox setup: the token carried none")
	}
	return sid.String(), nil
}
