package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createSecureTemp applies the token file's protected DACL at creation time.
// Windows ignores Unix owner-only mode bits, and tightening an inherited ACL
// after os.CreateTemp would leave a race in which another user could open the
// empty file before the token is written. CREATE_NEW with a security descriptor
// makes the current user, SYSTEM, and local administrators the only principals
// that can obtain a handle.
func createSecureTemp(dir string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)(A;;GA;;;BA)")
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	for {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, ".config-"+hex.EncodeToString(random[:])+".toml.tmp")
		name16, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(
			name16,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			&attributes,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		runtime.KeepAlive(descriptor)
		if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), name), nil
	}
}
