package credstore

import (
	"fmt"
	"time"
)

// credentialLockTimeout bounds how long acquiring the credential-store lock
// waits for a holder before giving up.
//
// It exists to agree with lockProviderWrite's providerWriteLockTimeout in
// internal/config. Those two locks are taken by the same provider operations,
// so opposite policies would mean a wedged holder reports "transaction is busy"
// through one lock and hangs forever with nothing on screen through the other.
// Neither lock is ever reclaimed by age — a credential backend may legitimately
// keep a holder blocked — so the timeout reports contention rather than
// stealing the lock.
var credentialLockTimeout = 5 * time.Second

// credentialLockRetryInterval paces the non-blocking retries. Both platforms
// poll rather than block so the deadline above is enforceable at all.
const credentialLockRetryInterval = 10 * time.Millisecond

// credentialLockBusyError names the lock file: the lock is never broken by age,
// so retrying alone never clears a stranded one and the user otherwise has no
// way to find the file to remove.
func credentialLockBusyError(path string) error {
	return fmt.Errorf("credstore: credential store is busy; retry the operation, or remove %s if no other Zero process is running", path)
}
