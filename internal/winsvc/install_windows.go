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

// Install registers the service, or updates the registration in place when
// the service already exists (installer re-runs for upgrades). A newly
// created service that cannot be fully configured is removed again.
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
	_, err := applySettings(s, c.PreshutdownTimeout)
	return err
}

// EnsureSettings brings an installed service's own SCM settings (delayed
// automatic start, recovery actions, pre-shutdown timeout) to what Install
// sets, so an MSI install, a hand install, and an upgrade converge (ADR
// 0012). The service calls it for itself at every start. It returns the
// settings it changed; a failure on one setting does not skip the others.
func EnsureSettings(name string, preshutdown time.Duration) ([]string, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("winsvc: connect to SCM: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return nil, fmt.Errorf("winsvc: open service %s: %w", name, err)
	}
	defer s.Close()
	return applySettings(s, preshutdown)
}

// applySettings is the one definition of the settings shared by Install and
// EnsureSettings. Each is read first and written only when it differs.
func applySettings(s *mgr.Service, preshutdown time.Duration) ([]string, error) {
	var changed []string
	var errs []error
	fail := func(what string, err error) { errs = append(errs, fmt.Errorf("winsvc: %s: %w", what, err)) }

	if cfg, err := s.Config(); err != nil {
		fail("read service config", err)
	} else {
		if cfg.StartType != mgr.StartAutomatic {
			n := uint32(windows.SERVICE_NO_CHANGE)
			if err := windows.ChangeServiceConfig(s.Handle, n, mgr.StartAutomatic, n, nil, nil, nil, nil, nil, nil, nil); err != nil {
				fail("automatic start", err)
			} else {
				changed = append(changed, "automatic start")
			}
		}
		if !cfg.DelayedAutoStart {
			info := windows.SERVICE_DELAYED_AUTO_START_INFO{IsDelayedAutoStartUp: 1}
			if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_DELAYED_AUTO_START_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
				fail("delayed automatic start", err)
			} else {
				changed = append(changed, "delayed automatic start")
			}
		}
	}

	want, reset := MgrRecoveryActions(), uint32(RecoveryResetPeriod/time.Second)
	cur, errA := s.RecoveryActions()
	curReset, errR := s.ResetPeriod()
	if errA != nil || errR != nil || curReset != reset || !sameRecovery(cur, want) {
		if err := s.SetRecoveryActions(want, reset); err != nil {
			fail("recovery actions", err)
		} else {
			changed = append(changed, "recovery actions")
		}
	}
	// Apply recovery when the service stops with an error exit code, not
	// only when the process crashes.
	if on, err := s.RecoveryActionsOnNonCrashFailures(); err != nil || !on {
		if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			fail("recovery on non-crash failures", err)
		} else {
			changed = append(changed, "recovery on non-crash failures")
		}
	}

	if preshutdown <= 0 {
		preshutdown = DefaultPreshutdownTimeout
	}
	// SERVICE_PRESHUTDOWN_INFO: a single DWORD timeout in milliseconds.
	ms := uint32(preshutdown / time.Millisecond)
	if cur, err := preshutdownTimeout(s); err != nil || cur != ms {
		info := struct{ Timeout uint32 }{ms}
		if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
			fail("preshutdown timeout", err)
		} else {
			changed = append(changed, "preshutdown timeout")
		}
	}
	return changed, errors.Join(errs...)
}

func sameRecovery(a, b []mgr.RecoveryAction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Delay != b[i].Delay {
			return false
		}
	}
	return true
}

func preshutdownTimeout(s *mgr.Service) (uint32, error) {
	var buf [16]byte
	var need uint32
	if err := windows.QueryServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, &buf[0], uint32(len(buf)), &need); err != nil {
		return 0, err
	}
	return *(*uint32)(unsafe.Pointer(&buf[0])), nil
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

// Restart stops the service (waiting for it) and starts it again. Used by
// the detached helper behind the dashboard's Restart service action.
func Restart(name string, stopTimeout time.Duration) error {
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
	if err := s.Start(); err != nil {
		return fmt.Errorf("winsvc: start service: %w", err)
	}
	return nil
}
