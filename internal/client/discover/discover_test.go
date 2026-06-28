package discover

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jssblck/akari/internal/config"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	codexDir := filepath.Join(dir, "codex")
	piDir := filepath.Join(dir, "pi")

	// Claude: nested jsonl under projects, including a subagents dir.
	write(t, filepath.Join(claudeDir, "proj-a", "sess1.jsonl"))
	write(t, filepath.Join(claudeDir, "proj-a", "subagents", "sess2.jsonl"))
	write(t, filepath.Join(claudeDir, "proj-a", "notes.txt")) // ignored: not jsonl

	// Codex: only rollout-*.jsonl count.
	write(t, filepath.Join(codexDir, "2024", "01", "rollout-2024-01-01-abc.jsonl"))
	write(t, filepath.Join(codexDir, "2024", "01", "other.jsonl")) // ignored: no rollout- prefix

	// pi.
	write(t, filepath.Join(piDir, "encoded-cwd", "sessX.jsonl"))

	roots := []Root{{"claude", claudeDir}, {"codex", codexDir}, {"pi", piDir}, {"pi", filepath.Join(dir, "missing")}}
	files, err := Discover(roots)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}      // path base -> agent
	fileRoot := map[string]string{} // path base -> discovery root
	for _, f := range files {
		got[filepath.Base(f.Path)] = f.Agent
		fileRoot[filepath.Base(f.Path)] = f.Root
	}
	want := map[string]string{
		"sess1.jsonl":                  "claude",
		"sess2.jsonl":                  "claude",
		"rollout-2024-01-01-abc.jsonl": "codex",
		"sessX.jsonl":                  "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %v, want %v", got, want)
	}
	for name, agent := range want {
		if got[name] != agent {
			t.Errorf("%s: agent %q, want %q", name, got[name], agent)
		}
	}

	// Each file records the discovery root it was found under, so resolution can
	// derive an id from its location. The two claude files share the claude root.
	wantRoot := map[string]string{
		"sess1.jsonl":                  claudeDir,
		"sess2.jsonl":                  claudeDir,
		"rollout-2024-01-01-abc.jsonl": codexDir,
		"sessX.jsonl":                  piDir,
	}
	for name, dir := range wantRoot {
		if fileRoot[name] != dir {
			t.Errorf("%s: root %q, want %q", name, fileRoot[name], dir)
		}
	}
}

func TestRoots(t *testing.T) {
	home := filepath.Join("/home", "grace")
	cfg := config.Client{ExtraRoots: []config.ExtraRoot{{Agent: "pi", Path: "/extra/pi"}}}

	// No env overrides: standard roots plus the configured extra root.
	roots := Roots(cfg, func(string) string { return "" }, home)
	want := []Root{
		{"claude", filepath.Join(home, ".claude", "projects")},
		{"codex", filepath.Join(home, ".codex", "sessions")},
		{"codex", filepath.Join(home, ".codex", "archived_sessions")},
		{"pi", filepath.Join(home, ".pi", "agent", "sessions")},
		{"pi", "/extra/pi"},
	}
	if len(roots) != len(want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("root %d = %v, want %v", i, roots[i], want[i])
		}
	}

	// Env overrides win for the agents that define them.
	env := map[string]string{
		"CLAUDE_PROJECTS_DIR": "/custom/claude",
		"CODEX_SESSIONS_DIR":  "/custom/codex",
		"PI_DIR":              "/custom/pihome",
	}
	roots = Roots(config.Client{}, func(k string) string { return env[k] }, home)
	if roots[0] != (Root{"claude", "/custom/claude"}) {
		t.Errorf("claude override = %v", roots[0])
	}
	if roots[1] != (Root{"codex", "/custom/codex"}) {
		t.Errorf("codex override = %v", roots[1])
	}
	// PI_DIR points at the pi home; sessions live under agent/sessions.
	if roots[2] != (Root{"pi", filepath.Join("/custom/pihome", "agent", "sessions")}) {
		t.Errorf("pi override = %v", roots[2])
	}
}
