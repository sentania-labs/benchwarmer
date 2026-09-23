//go:build !linux && !windows

package procgroup

import "syscall"

func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
