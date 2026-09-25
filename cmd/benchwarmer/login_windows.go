package main

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wtsapi32                        = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
)

const (
	wtsUserName   = 5
	wtsDomainName = 7
)

func sessionString(session uint32, class uintptr) (string, error) {
	var buf *uint16
	var n uint32
	r, _, e := procWTSQuerySessionInformationW.Call(0, uintptr(session), class, uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return "", e
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf), nil
}

// checkSameUser refuses the tray's sign-in when the elevated helper runs as
// a different account from the person signed in to this session (an
// administrator typed their credentials into a standard user's UAC prompt).
// The resulting dashboard session would otherwise belong to the standard
// user.
func checkSameUser() error {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return err
	}
	user, err := sessionString(session, wtsUserName)
	if err != nil {
		return fmt.Errorf("read the signed-in user: %w", err)
	}
	domain, _ := sessionString(session, wtsDomainName)
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	acct, dom, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return err
	}
	if !strings.EqualFold(acct, user) || (domain != "" && !strings.EqualFold(dom, domain)) {
		return fmt.Errorf("sign-in approved as %s\\%s, but %s\\%s is signed in: sign in to Windows with an administrator account to change settings", dom, acct, domain, user)
	}
	return nil
}
