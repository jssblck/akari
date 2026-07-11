//go:build windows

package discover

import (
	"os"
	"syscall"
)

// isWindowsReparsePoint reports whether info's raw Windows file attributes mark
// it as a reparse point: a true NTFS symlink or, more importantly for root
// detection, a directory junction (mklink /J). Go's Lstat sets ModeSymlink only
// for the former, so a junction otherwise surfaces as plain ModeIrregular and
// would be misclassified as neither a link nor a directory. The attributes are
// already present on the Lstat result's Sys(), so this needs no extra syscall.
func isWindowsReparsePoint(info os.FileInfo) bool {
	sys, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return sys.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
