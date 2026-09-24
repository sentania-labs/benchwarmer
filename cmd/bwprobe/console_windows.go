//go:build windows

package main

import "golang.org/x/sys/windows"

var procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// hasConsole reports whether this process is attached to a console, which
// Ctrl+C delivery to the runtime requires.
func hasConsole() bool {
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}
