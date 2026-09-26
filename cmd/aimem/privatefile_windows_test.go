//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// setFileDACL replaces path's DACL with a protected one from SDDL.
func setFileDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// makeWorldReadable grants Everyone read access to path.
func makeWorldReadable(t *testing.T, path string) {
	t.Helper()
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	setFileDACL(t, path, "D:P(A;;FA;;;"+me.String()+")(A;;FR;;;WD)")
}

func TestPrivateFileEffectiveAccess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	f, err := createPrivateFile(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := checkPrivateFile(p); err != nil {
		t.Fatalf("private file refused: %v", err)
	}
	// The created DACL is protected and names the current user only.
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("created DACL is not owner-only: %v", sd.String())
	}
	if ctl, _, _ := sd.Control(); ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("created DACL inherits from the directory")
	}
	if _, err := createPrivateFile(p); err == nil {
		t.Fatal("an existing file was reopened instead of refused")
	}
	me, _ := currentUserSID()
	for name, sddl := range map[string]string{
		"Everyone read":            "D:P(A;;FA;;;" + me.String() + ")(A;;FR;;;WD)",
		"Users read":               "D:P(A;;FA;;;" + me.String() + ")(A;;FR;;;BU)",
		"Authenticated Users full": "D:P(A;;FA;;;" + me.String() + ")(A;;FA;;;AU)",
		"Everyone change ACL":      "D:P(A;;FA;;;" + me.String() + ")(A;;WD;;;WD)",
	} {
		setFileDACL(t, p, sddl)
		if err := checkPrivateFile(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A null DACL grants every account full access.
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateFile(p); err == nil || !strings.Contains(err.Error(), "no access control list") {
		t.Errorf("null DACL: %v", err)
	}
	// SYSTEM and Administrators may keep access; a deny entry is ignored.
	setFileDACL(t, p, "D:P(A;;FA;;;"+me.String()+")(A;;FA;;;SY)(A;;FA;;;BA)(D;;FR;;;WD)")
	if err := checkPrivateFile(p); err != nil {
		t.Errorf("owner, SYSTEM and Administrators refused: %v", err)
	}
	// An inherit-only entry never applies to the file itself.
	setFileDACL(t, p, "D:P(A;;FA;;;"+me.String()+")(A;IO;FR;;;WD)")
	if err := checkPrivateFile(p); err != nil {
		t.Errorf("inherit-only entry treated as access: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}
