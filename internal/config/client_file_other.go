//go:build !windows

package config

import "os"

func createSecureTemp(dir string) (*os.File, error) {
	file, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}
