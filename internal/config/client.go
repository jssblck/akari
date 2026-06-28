package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Client holds the akari client configuration. All of it lives in one TOML file
// at the platform per-user config location; the client keeps no other on-disk
// state and defines no environment variables of its own.
type Client struct {
	// ServerURL is the base URL of the akari server, e.g. https://akari.example.
	ServerURL string `toml:"server_url"`
	// Token is an API token (ingest or full scope) used as a Bearer credential.
	Token string `toml:"token"`
	// ExtraRoots are additional session directories to discover beyond each
	// agent's standard roots.
	ExtraRoots []ExtraRoot `toml:"extra_roots"`
	// Excludes are glob patterns of paths to ignore while watching (used in watch
	// mode). Empty means watch everything discovered.
	Excludes []string `toml:"excludes"`
}

// ExtraRoot is a user-configured session directory for one agent.
type ExtraRoot struct {
	Agent string `toml:"agent"` // claude | codex | pi
	Path  string `toml:"path"`
}

// DefaultClientPath returns the platform-standard config file path:
// ~/.config/akari/config.toml on Linux, ~/Library/Application
// Support/akari/config.toml on macOS, %AppData%\akari\config.toml on Windows.
func DefaultClientPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "akari", "config.toml"), nil
}

func resolveClientPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return DefaultClientPath()
}

// ReadClient decodes the config without validating it. It reports whether the
// file existed (a missing file is not an error: callers like login start from a
// blank config) and returns an error only on a genuine read or parse failure, so
// a corrupt file is never silently treated as empty.
func ReadClient(path string) (cfg Client, exists bool, err error) {
	path, err = resolveClientPath(path)
	if err != nil {
		return Client{}, false, err
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return Client{}, false, nil
		}
		return Client{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	return cfg, true, nil
}

// LoadClient reads and validates the client config. An empty path uses
// DefaultClientPath. A missing file is a usable error directing the user to log
// in.
func LoadClient(path string) (Client, error) {
	resolved, err := resolveClientPath(path)
	if err != nil {
		return Client{}, err
	}
	c, exists, err := ReadClient(resolved)
	if err != nil {
		return Client{}, err
	}
	if !exists {
		return Client{}, fmt.Errorf("no config at %s: run `akari login` or create it", resolved)
	}
	if c.ServerURL == "" {
		return Client{}, fmt.Errorf("config %s: server_url is required", resolved)
	}
	if c.Token == "" {
		return Client{}, fmt.Errorf("config %s: token is required", resolved)
	}
	for i, r := range c.ExtraRoots {
		if r.Agent == "" || r.Path == "" {
			return Client{}, fmt.Errorf("config %s: extra_roots[%d] needs both agent and path", resolved, i)
		}
	}
	return c, nil
}

// SaveClient writes the config to path (creating parent directories) with
// owner-only permissions since it holds an API token. It writes to a temporary
// file and atomically renames it into place, so a crash or write error cannot
// truncate or destroy an existing config.
func SaveClient(path string, c Client) error {
	path, err := resolveClientPath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(c); err != nil {
		tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}
