//go:build darwin

package background

import "golang.org/x/sys/unix"

// szomb is sys/proc.h SZOMB: the process has exited and is waiting to be reaped.
const szomb int8 = 5

// leaderWaitableExited reports whether pid has already exited and is waiting
// to be reaped. This is the POSIX analogue of Windows GetExitCodeProcess
// returning a value other than STILL_ACTIVE: the leader is a zombie, waitable
// as exited, without collecting it. A true result does not mean the process
// group is empty — descendants may still be running.
//
// Darwin has no /proc and x/sys/unix does not wrap waitid here, so the probe
// is kern.proc.pid via sysctl. It does not use ps, which is the #862 failure
// mode when PATH cannot resolve ps.
func leaderWaitableExited(pid int) bool {
	if pid <= 1 {
		return false
	}
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return kinfo.Proc.P_stat == szomb
}
