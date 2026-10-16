//go:build linux

package background

import (
	"bytes"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// POSIX waitid si_code values for an already-exited child (bits/waitflags.h).
const (
	cldExited int32 = 1
	cldKilled int32 = 2
	cldDumped int32 = 3
)

// leaderWaitableExited reports whether pid has already exited and is waiting
// to be reaped. This is the POSIX analogue of Windows GetExitCodeProcess
// returning a value other than STILL_ACTIVE: the leader is waitable as exited
// (typically a zombie) without collecting it. A true result does not mean the
// process group is empty — descendants may still be running.
//
// Detection does not use ps. waitid(WNOHANG|WNOWAIT) asks the kernel whether
// an exit status is already available. /proc/<pid>/stat state Z is the
// fallback when waitid cannot answer (for example if procfs is the only
// usable source). Either path is independent of PATH, which is the #862
// failure mode: empty PATH makes ps unresolvable and kill(0) still succeeds
// against a zombie.
func leaderWaitableExited(pid int) bool {
	if pid <= 1 {
		return false
	}
	if waitidAlreadyExited(pid) {
		return true
	}
	return procIsZombie(pid)
}

func waitidAlreadyExited(pid int) bool {
	var info unix.Siginfo
	err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
	if err != nil {
		return false
	}
	switch info.Code {
	case cldExited, cldKilled, cldDumped:
		return true
	}
	return false
}

func procIsZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// /proc/<pid>/stat: pid (comm) state ... — comm may contain spaces or
	// parentheses, so the state is the first field after the last ')'.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+1 >= len(data) {
		return false
	}
	rest := bytes.TrimSpace(data[i+1:])
	return len(rest) > 0 && rest[0] == 'Z'
}
