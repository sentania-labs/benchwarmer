package service

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// requestRestart starts a detached `benchwarmer service restart`, which
// stops this service through the SCM (releasing the GPU the normal way) and
// starts it again. The helper outlives this process; the short delay lets
// the API answer first.
func (s *Service) requestRestart() error {
	if s.o.ServiceName == "" {
		return errors.New("not running as a Windows service; restart it the way it was started")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "service", "restart", "--delay", "2s", "--data", s.o.DataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
