//go:build windows

package selfupdate

import (
	"fmt"
	"os"
)

// Replace installs newPath at target, replacing the running binary.
//
// Windows will not let a running .exe be overwritten in place, but it does allow
// the running image to be renamed. So move the live binary aside to a ".old"
// sibling, then move the new binary into its place. The current process keeps
// running from the renamed-away image; the next launch runs the new binary. The
// ".old" file cannot be deleted while it is still the running image, so it is
// left for CleanupOld to remove on the next run. newPath must be on the same
// filesystem as target (the caller extracts it into target's directory).
func Replace(target, newPath string) error {
	old := target + ".old"
	// Clear a stale .old from a previous update so the rename below has a clear
	// destination; ignore the error when it is still locked.
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("move running binary aside: %w", err)
	}
	if err := os.Rename(newPath, target); err != nil {
		// Best effort: put the original back so the install is not left broken.
		_ = os.Rename(old, target)
		return fmt.Errorf("install new binary at %s: %w", target, err)
	}
	// Usually still locked (it is the running image); CleanupOld gets it later.
	_ = os.Remove(old)
	return nil
}

// CleanupOld removes the ".old" file a prior Windows update left behind, once it
// is no longer the running image. Errors are ignored: a still-locked file is
// retried on the next run.
func CleanupOld(target string) {
	_ = os.Remove(target + ".old")
}
