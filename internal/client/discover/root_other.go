//go:build !windows

package discover

import "os"

// isWindowsReparsePoint always reports false outside Windows: only NTFS has
// directory junctions, and a real symlink is already caught by Go's ModeSymlink
// on every platform this build targets.
func isWindowsReparsePoint(os.FileInfo) bool {
	return false
}
