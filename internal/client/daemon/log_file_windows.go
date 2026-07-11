//go:build windows

package daemon

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openSecureLogFile protects a new log at creation time so inherited directory
// permissions never expose even an empty file. Existing logs are secured before
// they are opened for append.
func openSecureLogFile(path string) (*os.File, error) {
	descriptor, err := daemonLogSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ,
		&attributes,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	runtime.KeepAlive(descriptor)
	if err == nil {
		return os.NewFile(uintptr(handle), path), nil
	}
	if err != windows.ERROR_FILE_EXISTS && err != windows.ERROR_ALREADY_EXISTS {
		return nil, err
	}
	if err := secureLogPath(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
}

func secureLogPath(path string) error {
	descriptor, err := daemonLogSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
	runtime.KeepAlive(descriptor)
	return err
}

func daemonLogSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	return windows.SecurityDescriptorFromString(
		"O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)(A;;GA;;;BA)")
}
