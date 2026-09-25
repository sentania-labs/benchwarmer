//go:build windows

package provision

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var platform fsOps = winOps{}

// winOps works through handles opened with FILE_FLAG_OPEN_REPARSE_POINT,
// so a junction or link is examined itself and never followed, and a path
// swapped for one between the check and the write is caught on the handle.
type winOps struct{}

func open(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// BACKUP_SEMANTICS opens folders, and with the backup and restore
	// privileges enabled, files whose planted DACL shuts out SYSTEM.
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func info(h windows.Handle) (entry, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return entry{}, err
	}
	e := entry{Dir: fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
		Reparse: fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, Links: fi.NumberOfLinks}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return entry{}, fmt.Errorf("read owner: %w", err)
	}
	if o, _, err := sd.Owner(); err == nil && o != nil {
		e.Owner = o.String()
	}
	return e, nil
}

func (winOps) inspect(path string) (entry, error) {
	// A missing path's error matches fs.ErrNotExist.
	h, err := open(path, windows.READ_CONTROL)
	if err != nil {
		return entry{}, err
	}
	defer windows.CloseHandle(h)
	return info(h)
}

// secure compares owner and DACL on the handle and writes only what
// differs, then reads both back to confirm.
func (winOps) secure(t Target) (bool, error) {
	wantDACL, err := parseDACL(t.SDDL)
	if err != nil {
		return false, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	h, err := open(t.Path, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return false, fmt.Errorf("open: %w", err)
	}
	defer windows.CloseHandle(h)
	e, err := info(h)
	if err != nil {
		return false, err
	}
	if e.Reparse {
		return false, errors.New("became a junction or link; not following it")
	}
	ownerOK, daclOK, err := check(h, e.Owner, t)
	if err != nil {
		return false, err
	}
	if ownerOK && daclOK {
		return false, nil
	}
	var si windows.SECURITY_INFORMATION
	var owner *windows.SID
	var dacl *windows.ACL
	if !ownerOK {
		// SYSTEM's token holds Administrators as a possible owner, so no
		// privilege is needed to assign it.
		si |= windows.OWNER_SECURITY_INFORMATION
		owner = admins
	}
	if !daclOK {
		si |= windows.DACL_SECURITY_INFORMATION
		if t.Protected {
			si |= windows.PROTECTED_DACL_SECURITY_INFORMATION
		} else {
			si |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
		}
		dacl = wantDACL
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, si, owner, nil, dacl, nil); err != nil {
		return false, fmt.Errorf("set: %w", err)
	}
	e, err = info(h)
	if err != nil {
		return true, fmt.Errorf("verify: %w", err)
	}
	if ownerOK, daclOK, err = check(h, e.Owner, t); err != nil {
		return true, fmt.Errorf("verify: %w", err)
	} else if !ownerOK || !daclOK {
		return true, errors.New("verify: owner or ACL differs after it was set")
	}
	return true, nil
}

// check reports whether the owner is Administrators and the DACL has
// exactly t's explicit ACEs and protection (or one of t.Accept).
func check(h windows.Handle, owner string, t Target) (ownerOK, daclOK bool, err error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, false, fmt.Errorf("read: %w", err)
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return false, false, fmt.Errorf("read: %w", err)
	}
	cur, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && cur == nil) {
		// No DACL or a NULL DACL: open to everyone.
		return owner == SIDAdministrators, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read: %w", err)
	}
	got, err := explicitACEs(cur)
	if err != nil {
		return false, false, err
	}
	protected := ctl&windows.SE_DACL_PROTECTED != 0
	for i, s := range append([]string{t.SDDL}, t.Accept...) {
		wantProt := t.Protected
		if i > 0 {
			wantProt = strings.HasPrefix(s, "D:P")
		}
		if protected != wantProt {
			continue
		}
		d, err := parseDACL(s)
		if err != nil {
			return false, false, err
		}
		want, err := explicitACEs(d)
		if err != nil {
			return false, false, err
		}
		if sameACEs(got, want) {
			return owner == SIDAdministrators, true, nil
		}
	}
	return owner == SIDAdministrators, false, nil
}

func parseDACL(sddl string) (*windows.ACL, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", sddl, err)
	}
	d, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", sddl, err)
	}
	if d == nil {
		// A NULL DACL grants everyone full access; never write one.
		return nil, fmt.Errorf("%q yields a NULL DACL", sddl)
	}
	// The ACL points into sd's Go memory, which stays reachable through
	// the returned pointer's allocation.
	return d, nil
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

// privileges enables backup, restore, and take-ownership on the process
// token for the duration of provisioning. LocalSystem holds them; they
// let the service open and re-own files whose planted DACL shuts it out.
// The undo restores the previous state.
func (winOps) privileges() (func(), error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return func() {}, err
	}
	var prevs []windows.Tokenprivileges
	var errs []error
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeTakeOwnershipPrivilege"} {
		var luid windows.LUID
		n, _ := windows.UTF16PtrFromString(name)
		if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		var prev windows.Tokenprivileges
		var ret uint32
		// A privilege the token lacks is silently not enabled; the
		// operations that need it then fail and are reported.
		if err := windows.AdjustTokenPrivileges(tok, false, &tp, uint32(unsafe.Sizeof(prev)), &prev, &ret); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if prev.PrivilegeCount > 0 {
			prevs = append(prevs, prev)
		}
	}
	return func() {
		for i := range prevs {
			_ = windows.AdjustTokenPrivileges(tok, false, &prevs[i], 0, nil, nil)
		}
		tok.Close()
	}, errors.Join(errs...)
}
