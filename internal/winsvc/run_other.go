//go:build !windows

package winsvc

import "time"

// Run runs h as a console process; there is no service manager off Windows.
func Run(o Options, h Handler) error { return RunConsole(o, h) }

// IsService reports whether the process runs under the service control
// manager: never, off Windows.
func IsService() bool { return false }

// EnsureSettings has no service registration to maintain off Windows.
func EnsureSettings(string, time.Duration) ([]string, error) { return nil, nil }
