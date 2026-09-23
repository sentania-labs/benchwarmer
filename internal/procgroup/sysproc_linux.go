//go:build linux

package procgroup

import "syscall"

// Pdeathsig fires when the OS thread that forked the child exits, not the
// process; acceptable for the development-only Unix implementation.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
