//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

// createPrivateFile creates path exclusively (CREATE_NEW) with a protected
// DACL that grants the current user alone; nothing is inherited from the
// directory.
func createPrivateFile(path string) (*os.File, error) {
	me, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + me.String() + ")")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// privateFileRisky are the rights that let an account read the file, or
// grant itself access to read it.
const privateFileRisky = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL |
	windows.WRITE_DAC | windows.WRITE_OWNER

// checkPrivateFile checks the file's effective access, not its mode bits: the
// owner must be the current user, SYSTEM or Administrators, and every allow
// entry that grants read (or the right to change the ACL or owner) must name
// one of those accounts. A null DACL, which grants everyone full access, is
// refused, and so is any entry type the check does not understand.
func checkPrivateFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cannot read the access control list of %s: %w", path, err)
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	trusted := []*windows.SID{me}
	for _, k := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(k)
		if err != nil {
			return err
		}
		trusted = append(trusted, sid)
	}
	isTrusted := func(sid *windows.SID) bool {
		for _, t := range trusted {
			if sid.Equals(t) {
				return true
			}
		}
		return false
	}
	fix := fmt.Sprintf("restrict it to your account, for example: icacls \"%s\" /inheritance:r /grant:r \"%%USERNAME%%:F\"", path)
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !isTrusted(owner) {
		return fmt.Errorf("%s is owned by another account; %s", path, fix)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("%s has no access control list, so every account can read it; %s", path, fix)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to children only, never to this file
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("%s has an access entry of a type this check does not understand (%d); %s", path, ace.Header.AceType, fix)
		}
		if uint32(ace.Mask)&privateFileRisky == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !isTrusted(sid) {
			return fmt.Errorf("%s is readable by %s; %s", path, sid.String(), fix)
		}
	}
	return nil
}
