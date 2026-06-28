//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
)

// Replace installs newPath at target, replacing the running binary.
//
// On Unix a running executable's file can be replaced while it runs: the kernel
// keeps the old inode mapped for the live process, and the rename atomically
// swaps the directory entry so the next launch picks up the new binary. newPath
// must be on the same filesystem as target (the caller extracts it into target's
// directory) so the rename does not fail with a cross-device error.
func Replace(target, newPath string) error {
	if err := os.Chmod(newPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(newPath, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}

// CleanupOld is a no-op on Unix; only the Windows path leaves a stale file
// behind after an update.
func CleanupOld(target string) {}
