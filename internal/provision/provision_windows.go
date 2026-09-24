//go:build windows

package provision

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyACL compares the path's explicit ACEs and inheritance protection
// with t and rewrites the DACL only when they differ, then reads it back to
// confirm. It reports whether it rewrote. Inherited ACEs are not compared:
// they follow from the parent, which is applied first.
func applyACL(t Target) (bool, error) {
	want, err := windows.SecurityDescriptorFromString(t.SDDL)
	if err != nil {
		return false, fmt.Errorf("parse %q: %w", t.SDDL, err)
	}
	wantDACL, _, err := want.DACL()
	if err != nil {
		return false, fmt.Errorf("parse %q: %w", t.SDDL, err)
	}
	if wantDACL == nil {
		// A NULL DACL grants everyone full access; never write one.
		return false, fmt.Errorf("%q yields a NULL DACL", t.SDDL)
	}
	wantACEs, err := explicitACEs(wantDACL)
	if err != nil {
		return false, err
	}

	if ok, err := matches(t.Path, wantACEs, t.Protected); err != nil {
		return false, err
	} else if ok {
		return false, nil
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if t.Protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	// Existing children pick up the new inheritable ACEs automatically.
	if err := windows.SetNamedSecurityInfo(t.Path, windows.SE_FILE_OBJECT, info, nil, nil, wantDACL, nil); err != nil {
		return false, fmt.Errorf("set: %w", err)
	}
	runtime.KeepAlive(want)
	if ok, err := matches(t.Path, wantACEs, t.Protected); err != nil {
		return true, fmt.Errorf("verify: %w", err)
	} else if !ok {
		return true, errors.New("verify: ACL differs after it was set")
	}
	return true, nil
}

// matches reports whether path's DACL has exactly the wanted explicit ACEs
// and inheritance protection.
func matches(path string, want []ace, protected bool) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	defer runtime.KeepAlive(sd)
	ctl, _, err := sd.Control()
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	if (ctl&windows.SE_DACL_PROTECTED != 0) != protected {
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		// No DACL or a NULL DACL: open to everyone.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}
	got, err := explicitACEs(dacl)
	if err != nil {
		return false, err
	}
	return sameACEs(got, want), nil
}

// explicitACEs lists the ACEs not inherited from a parent. The SID is read
// for the allow and deny types only; any other type still differs from the
// wanted set by type, which is all the comparison needs.
func explicitACEs(acl *windows.ACL) ([]ace, error) {
	var out []ace
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var a *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &a); err != nil {
			return nil, fmt.Errorf("read ACE %d: %w", i, err)
		}
		if a.Header.AceFlags&windows.INHERITED_ACE != 0 {
			continue
		}
		e := ace{Type: a.Header.AceType, Flags: a.Header.AceFlags, Mask: uint32(a.Mask)}
		if e.Type == windows.ACCESS_ALLOWED_ACE_TYPE || e.Type == windows.ACCESS_DENIED_ACE_TYPE {
			e.SID = (*windows.SID)(unsafe.Pointer(&a.SidStart)).String()
		}
		out = append(out, e)
	}
	return out, nil
}
