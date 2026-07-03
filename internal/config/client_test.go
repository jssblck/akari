package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// errAny stands in for any hostname lookup failure in ResolveMachine's tests.
var errAny = errors.New("hostname lookup failed")

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := Client{
		ServerURL:  "https://akari.example",
		Token:      "secret-token",
		ExtraRoots: []ExtraRoot{{Agent: "pi", Path: "/extra/pi"}},
		Excludes:   []string{"**/tmp/**"},
	}
	if err := SaveClient(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClient(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerURL != in.ServerURL || got.Token != in.Token {
		t.Errorf("round trip = %+v", got)
	}
	if len(got.ExtraRoots) != 1 || got.ExtraRoots[0] != in.ExtraRoots[0] {
		t.Errorf("extra roots not preserved: %+v", got.ExtraRoots)
	}
	if len(got.Excludes) != 1 || got.Excludes[0] != "**/tmp/**" {
		t.Errorf("excludes not preserved: %+v", got.Excludes)
	}
}

func TestResolveMachine(t *testing.T) {
	const hostname = "host-from-os"
	okHost := func() (string, error) { return hostname, nil }
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}

	cases := []struct {
		name string
		cfg  Client
		env  map[string]string
		host func() (string, error)
		want string
	}{
		{
			name: "hostname is the default",
			host: okHost,
			want: hostname,
		},
		{
			name: "config overrides hostname",
			cfg:  Client{Machine: "sandbox-pool"},
			host: okHost,
			want: "sandbox-pool",
		},
		{
			name: "env overrides config and hostname",
			cfg:  Client{Machine: "sandbox-pool"},
			env:  map[string]string{MachineEnvVar: "ci"},
			host: okHost,
			want: "ci",
		},
		{
			name: "blank env falls through to config",
			cfg:  Client{Machine: "sandbox-pool"},
			env:  map[string]string{MachineEnvVar: "   "},
			host: okHost,
			want: "sandbox-pool",
		},
		{
			name: "blank config falls through to hostname",
			cfg:  Client{Machine: "  "},
			host: okHost,
			want: hostname,
		},
		{
			name: "values are trimmed",
			env:  map[string]string{MachineEnvVar: "  ci  "},
			host: okHost,
			want: "ci",
		},
		{
			name: "a hostname error yields an empty machine, as before",
			host: func() (string, error) { return "", errAny },
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveMachine(tc.cfg, env(tc.env), tc.host)
			if got != tc.want {
				t.Errorf("ResolveMachine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSaveLoadPreservesMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	in := Client{ServerURL: "https://akari.example", Token: "t", Machine: "ci"}
	if err := SaveClient(path, in); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClient(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Machine != "ci" {
		t.Errorf("machine not round-tripped: %q", got.Machine)
	}
}

func TestReadClientMissing(t *testing.T) {
	_, exists, err := ReadClient(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing config should not error: %v", err)
	}
	if exists {
		t.Error("missing config reported as existing")
	}
}

func TestReadClientCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("this is = not [valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadClient(path); err == nil {
		t.Fatal("corrupt config should error, not be treated as empty")
	}
}

func TestLoadClientValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`server_url = "https://x"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClient(path); err == nil {
		t.Fatal("config without a token should fail validation")
	}
}

func TestSaveDoesNotDestroyOnRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	first := Client{ServerURL: "https://a", Token: "t1", ExtraRoots: []ExtraRoot{{Agent: "pi", Path: "/p"}}}
	if err := SaveClient(path, first); err != nil {
		t.Fatal(err)
	}
	// A second save replaces the file atomically and leaves no stray temp files.
	if err := SaveClient(path, Client{ServerURL: "https://b", Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClient(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerURL != "https://b" || got.Token != "t2" {
		t.Errorf("second save not applied: %+v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}
