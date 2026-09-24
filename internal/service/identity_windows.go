//go:build windows

package service

import "golang.org/x/sys/windows"

const requiresSystemForLocalService = true

func isLocalSystem() bool {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return tu.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}
