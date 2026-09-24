package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteEx = shell32.NewProc("ShellExecuteExW")
)

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	size       uint32
	mask       uint32
	hwnd       uintptr
	verb       *uint16
	file       *uint16
	parameters *uint16
	directory  *uint16
	show       int32
	instApp    uintptr
	idList     uintptr
	class      *uint16
	keyClass   uintptr
	hotKey     uint32
	iconOrMon  uintptr
	process    windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x40
	swHide                = 0
)

// signIn opens the dashboard signed in as administrator (ADR 0012). It
// makes a one-time code, has `benchwarmer login` register it through a UAC
// prompt (only an administrator can read the management token), and then
// opens the dashboard from this unelevated process, so the browser does not
// run elevated. The code reaches the page in the URL fragment, which the
// browser never sends to the server.
func signIn(base string) error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bw := filepath.Join(filepath.Dir(exe), "benchwarmer.exe")
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(bw)
	params, _ := windows.UTF16PtrFromString("login --code " + code)
	info := shellExecuteInfo{mask: seeMaskNoCloseProcess, verb: verb, file: file, parameters: params, show: swHide}
	info.size = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return fmt.Errorf("start elevated sign-in: %w", e) // includes "cancelled by the user"
	}
	defer windows.CloseHandle(info.process)
	if _, err := windows.WaitForSingleObject(info.process, 60*1000); err != nil {
		return err
	}
	var code32 uint32
	if err := windows.GetExitCodeProcess(info.process, &code32); err != nil {
		return err
	}
	if code32 != 0 {
		return fmt.Errorf("sign-in helper failed (exit %d); see the service log", code32)
	}
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", base+"/#signin="+code).Start()
}
