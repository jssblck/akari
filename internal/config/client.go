package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// MachineEnvVar overrides the machine identity a client reports for its sessions.
// It is the only environment variable akari itself defines (the agent root
// overrides discovery honors belong to the agents). It wins over the config's
// machine field, which in turn wins over the OS hostname; see ResolveMachine.
const MachineEnvVar = "AKARI_MACHINE"

// Client holds the akari client configuration. All of it lives in one TOML file
// at the platform per-user config location; the client keeps no other on-disk
// state and defines a single environment variable of its own (AKARI_MACHINE, see
// ResolveMachine).
type Client struct {
	// ServerURL is the base URL of the akari server, e.g. https://akari.example.
	ServerURL string `toml:"server_url"`
	// Token is an API token (ingest or full scope) used as a Bearer credential.
	Token string `toml:"token"`
	// Machine is the logical machine name reported for every session this client
	// uploads. Empty falls back to the OS hostname. Set it (at login or by hand)
	// to give a fleet of ephemeral or containerized hosts one stable identity
	// instead of leaking a distinct one-off hostname per run. AKARI_MACHINE
	// overrides it per run; see ResolveMachine.
	Machine string `toml:"machine"`
	// ExtraRoots are additional session directories to discover beyond each
	// agent's standard roots.
	ExtraRoots []ExtraRoot `toml:"extra_roots"`
	// Excludes are glob patterns of paths to skip during discovery, applied to
	// both `akari sync` and `akari watch`. Patterns match the full path with
	// forward slashes and `*`/`**` span separators (see discover.Excluder), so
	// `**/tmp/**` ignores any path with a `tmp` segment. Empty means discover
	// everything.
	Excludes []string `toml:"excludes"`
}

// ResolveMachine determines the machine identity a client reports for its
// sessions. Ephemeral and containerized hosts (CI jobs, autoscaled workers,
// throwaway dev containers) each get a distinct one-off hostname, so a fleet of
// them otherwise pollutes the machine facet with thousands of single-use values.
// An explicit identity lets that fleet share one stable logical machine (for
// example "ci" or "sandbox-pool").
//
// Precedence, highest first:
//  1. the AKARI_MACHINE environment variable, for a per-run override without
//     touching the config, the natural fit for ephemeral hosts, which set an env
//     var far more easily than they write a config file;
//  2. the machine field in the client config, set at login for a stable per-host
//     or per-fleet name;
//  3. the OS hostname, the historical default.
//
// A blank env var or config value falls through rather than reporting an empty
// machine, and surrounding whitespace is trimmed. hostname is injected (pass
// os.Hostname) so the fallback is testable; its error is ignored exactly as the
// bare `machine, _ := os.Hostname()` call sites it replaces did.
func ResolveMachine(cfg Client, env func(string) string, hostname func() (string, error)) string {
	if v := strings.TrimSpace(env(MachineEnvVar)); v != "" {
		return v
	}
	if v := strings.TrimSpace(cfg.Machine); v != "" {
		return v
	}
	h, _ := hostname()
	return h
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

// ReadClient decodes the config without checking required values. It reports
// whether the file existed (a missing file is not an error: callers like login
// start from a blank config), but rejects malformed TOML and unknown keys. A
// misspelled exclusion or root setting must not silently weaken backup policy.
func ReadClient(path string) (cfg Client, exists bool, err error) {
	path, err = resolveClientPath(path)
	if err != nil {
		return Client{}, false, err
	}
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return Client{}, false, nil
		}
		return Client{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		return Client{}, false, fmt.Errorf("read config %s: unknown keys: %s", path, strings.Join(keys, ", "))
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
		switch r.Agent {
		case "claude", "codex", "pi":
		default:
			return Client{}, fmt.Errorf("config %s: extra_roots[%d].agent must be claude, codex, or pi", resolved, i)
		}
	}
	return c, nil
}

// SaveClient writes the config to path (creating parent directories) with
// protected per-user permissions since it holds an API token. Unix grants only
// the owner; Windows also retains SYSTEM and local-administrator recovery
// access. The temporary file is protected at creation and atomically renamed,
// so a crash or write error cannot truncate or expose an existing config.
func SaveClient(path string, c Client) error {
	path, err := resolveClientPath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	tmp, err := createSecureTemp(dir)
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
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
