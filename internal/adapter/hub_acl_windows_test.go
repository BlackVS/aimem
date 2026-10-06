//go:build windows

package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"aimem/internal/privatefile"

	"golang.org/x/sys/windows"
)

// A state root whose folder grants Everyone read access, inherited by new
// files, as a shared or sandboxed account's folder can: hub.json saved
// there must still be owner-only, because its DACL is protected and set
// explicitly instead of inherited.
func TestSaveHubsIgnoresAnInheritedBroadACL(t *testing.T) {
	root := t.TempDir()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	// A file written the old way inherits the broad grant: the check sees it.
	probe := filepath.Join(root, "probe.json")
	if err := os.WriteFile(probe, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Check(probe); err == nil {
		t.Fatal("the folder's inherited Everyone grant did not reach a plainly written file; the test proves nothing")
	}
	if err := SaveHubs(root, map[string]*HubConfig{"h": {URL: "https://hub.example.test", Token: "t"}}, "h"); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Check(filepath.Join(root, "hub.json")); err != nil {
		t.Fatalf("hub.json inherited the folder's broad ACL: %v", err)
	}
}
