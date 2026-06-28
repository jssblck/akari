package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
