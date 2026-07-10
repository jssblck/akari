package main

import (
	"fmt"
	"path/filepath"
)

// resolveExecutableTarget returns the file the running executable ultimately
// names. Updating a symlink path itself can replace the link and leave the
// service's real binary stale, so resolution errors stop the update.
func resolveExecutableTarget(target string) (string, error) {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve the running binary path: %w", err)
	}
	return resolved, nil
}
