//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// InstallConfig describes the service registration.
type InstallConfig struct {
	Name        string
	DisplayName string
	Description string
	// ExePath is the service binary; Args are passed on every start.
	ExePath string
	Args    []string
	// Account is AccountVirtual (default) or AccountLocalSystem.
	Account string
	// PreshutdownTimeout is how long Windows waits for the service after
	// the pre-shutdown notification; zero uses DefaultPreshutdownTimeout.
	PreshutdownTimeout time.Duration
}

// MgrConfig returns the SCM configuration: automatic start (delayed), the
// resolved account, and an unrestricted service SID for virtual accounts.
func (c InstallConfig) MgrConfig() (mgr.Config, error) {
	name, sid, err := startName(c.Account, c.Name)
	if err != nil {
		return mgr.Config{}, err
	}
	mc := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      c.DisplayName,
		Description:      c.Description,
		ServiceStartName: name,
		DelayedAutoStart: true,
	}
	if sid {
		mc.SidType = windows.SERVICE_SID_TYPE_UNRESTRICTED
	}
	return mc, nil
}

// MgrRecoveryActions converts RecoveryPlan to SCM failure actions.
func MgrRecoveryActions() []mgr.RecoveryAction {
	var out []mgr.RecoveryAction
	for _, s := range RecoveryPlan() {
		a := mgr.RecoveryAction{Type: mgr.NoAction, Delay: s.Delay}
		if s.Restart {
			a.Type = mgr.ServiceRestart
		}
		out = append(out, a)
	}
	return out
}

// Install registers the service. It fails if a service with the name
// already exists; a partially configured service is removed again.
func Install(c InstallConfig) error {
	if c.Name == "" || c.ExePath == "" {
		return errors.New("winsvc: Name and ExePath are required")
	}
	mc, err := c.MgrConfig()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winsvc: connect to SCM: %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(c.Name); err == nil {
		// Upgrade: update the existing registration in place so the
		// installer can be re-run (ADR 0009).
		defer s.Close()
		cur, err := s.Config()
		if err != nil {
			return fmt.Errorf("winsvc: read existing service config: %w", err)
		}
		mc.BinaryPathName = binaryPath(c.ExePath, c.Args)
		mc.ServiceType = cur.ServiceType
		if err := s.UpdateConfig(mc); err != nil {
			return fmt.Errorf("winsvc: update service: %w", err)
		}
		return configure(s, c)
	}
	s, err := m.CreateService(c.Name, c.ExePath, mc, c.Args...)
	if err != nil {
		return fmt.Errorf("winsvc: create service: %w", err)
	}
	defer s.Close()
	if err := configure(s, c); err != nil {
		_ = s.Delete()
		return err
	}
	return nil
}

func configure(s *mgr.Service, c InstallConfig) error {
	if err := s.SetRecoveryActions(MgrRecoveryActions(), uint32(RecoveryResetPeriod/time.Second)); err != nil {
		return fmt.Errorf("winsvc: recovery actions: %w", err)
	}
	// Apply recovery when the service stops with an error exit code, not
	// only when the process crashes.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("winsvc: recovery on non-crash failures: %w", err)
	}
	pt := c.PreshutdownTimeout
	if pt <= 0 {
		pt = DefaultPreshutdownTimeout
	}
	// SERVICE_PRESHUTDOWN_INFO: a single DWORD timeout in milliseconds.
	info := struct{ Timeout uint32 }{uint32(pt / time.Millisecond)}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("winsvc: preshutdown timeout: %w", err)
	}
	return nil
}

// Remove stops the service if it is running (waiting up to stopTimeout) and
// deletes it. Deletion completes once every open handle is closed.
func Remove(name string, stopTimeout time.Duration) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("winsvc: connect to SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("winsvc: open service %s: %w", name, err)
	}
	defer s.Close()
	if err := stopAndWait(s, stopTimeout); err != nil {
		return err
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("winsvc: delete service: %w", err)
	}
	return nil
}

func stopAndWait(s *mgr.Service, timeout time.Duration) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("winsvc: query service: %w", err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("winsvc: stop service: %w", err)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("winsvc: query service: %w", err)
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("winsvc: service still in state %d after %s", st.State, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// binaryPath quotes the executable and arguments the way mgr.CreateService
// does, for UpdateConfig (which takes the full command line).
func binaryPath(exe string, args []string) string {
	b := syscall.EscapeArg(exe)
	for _, a := range args {
		b += " " + syscall.EscapeArg(a)
	}
	return b
}
