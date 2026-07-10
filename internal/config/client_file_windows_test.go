package config

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestSaveClientProtectsWindowsTokenACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := SaveClient(path, Client{ServerURL: "https://akari.example", Token: "secret"}); err != nil {
		t.Fatal(err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("config DACL inherits permissions from its directory")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		user.User.Sid.String():  true,
		system.String():         true,
		administrators.String(): true,
	}
	if int(dacl.AceCount) != len(want) {
		t.Fatalf("config DACL has %d entries, want %d", dacl.AceCount, len(want))
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		const fullControl = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE |
			windows.FILE_GENERIC_EXECUTE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&fullControl != fullControl {
			t.Fatalf("config DACL entry %d is not a full-control allow ACE: type=%d mask=%#x", i, ace.Header.AceType, ace.Mask)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !want[sid.String()] {
			t.Fatalf("config DACL grants unexpected SID %s", sid.String())
		}
		delete(want, sid.String())
	}
	if len(want) != 0 {
		t.Fatalf("config DACL is missing expected SIDs: %v", want)
	}
}
