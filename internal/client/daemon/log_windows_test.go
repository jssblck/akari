//go:build windows

package daemon

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestLogFilesHaveProtectedWindowsACLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	log, err := openLog(path, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Write([]byte("old!")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("new!")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{path, backupPath(path, 1)} {
		assertProtectedLogACL(t, name)
	}
}

func assertProtectedLogACL(t *testing.T, path string) {
	t.Helper()
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
		t.Fatalf("%s DACL inherits permissions", path)
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
		t.Fatalf("%s DACL has %d entries, want %d", path, dacl.AceCount, len(want))
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !want[sid.String()] {
			t.Fatalf("%s DACL contains unexpected entry for %s", path, sid.String())
		}
		delete(want, sid.String())
	}
	if len(want) != 0 {
		t.Fatalf("%s DACL is missing SIDs: %v", path, want)
	}
}
