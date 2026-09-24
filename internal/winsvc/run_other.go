//go:build !windows

package winsvc

// Run runs h as a console process; there is no service manager off Windows.
func Run(o Options, h Handler) error { return RunConsole(o, h) }
