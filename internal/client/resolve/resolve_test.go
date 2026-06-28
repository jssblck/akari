package resolve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/client/discover"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPeekHeader(t *testing.T) {
	dir := t.TempDir()

	claude := writeFile(t, dir, "claude.jsonl",
		`{"type":"user","cwd":"/home/grace/app","gitBranch":"main","sessionId":"c-123","message":{"content":"hi"}}`+"\n")
	codex := writeFile(t, dir, "rollout-codex.jsonl",
		`{"type":"session_meta","payload":{"id":"x-9","cwd":"/home/grace/api","git":{"branch":"dev"}}}`+"\n")
	pi := writeFile(t, dir, "pi.jsonl",
		`{"type":"session","id":"p-7","cwd":"/home/grace/proj"}`+"\n")

	cases := []struct {
		agent, path                 string
		wantCwd, wantBranch, wantID string
	}{
		{"claude", claude, "/home/grace/app", "main", "c-123"},
		{"codex", codex, "/home/grace/api", "dev", "x-9"},
		{"pi", pi, "/home/grace/proj", "", "p-7"},
	}
	for _, c := range cases {
		h, err := PeekHeader(discover.File{Agent: c.agent, Root: dir, Path: c.path})
		if err != nil {
			t.Fatalf("%s: %v", c.agent, err)
		}
		if h.Cwd != c.wantCwd || h.GitBranch != c.wantBranch || h.SourceID != c.wantID {
			t.Errorf("%s header = %+v", c.agent, h)
		}
	}
}

func TestPeekHeaderFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	// A pi file whose header omits an id: the source id falls back to the stem.
	path := writeFile(t, dir, "fallback-id.jsonl", `{"type":"session","cwd":"/x"}`+"\n")
	h, err := PeekHeader(discover.File{Agent: "pi", Root: dir, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if h.SourceID != "fallback-id" {
		t.Errorf("source id = %q, want fallback-id", h.SourceID)
	}
}

// TestClaudeSourceIDUnique is the regression guard for the source-id collision:
// a Claude main session file and every subagent and workflow file beneath it all
// record the same in-file sessionId, so before the fix they folded onto one
// server row and clobbered each other. Each file must now resolve to a distinct
// source id, with subagents kept grouped under their parent sessionId and the
// main file keeping the bare sessionId.
func TestClaudeSourceIDUnique(t *testing.T) {
	root := t.TempDir()
	const sid = "4a7929e8-5b80-48e6-8ccc-a8919c89cd6d"

	// All four files carry the parent sessionId in their first line, exactly as
	// Claude writes subagent and workflow files.
	line := fmt.Sprintf(`{"type":"user","cwd":"/home/ada/app","gitBranch":"main","sessionId":%q}`+"\n", sid)

	mustWrite := func(rel string) string {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	proj := "-home-ada-app"
	main := mustWrite(proj + "/" + sid + ".jsonl")
	subA := mustWrite(proj + "/" + sid + "/subagents/agent-ac2d35a2e8e2aff8e.jsonl")
	subB := mustWrite(proj + "/" + sid + "/subagents/agent-bb11ff00deadbeef0.jsonl")
	wfJournal1 := mustWrite(proj + "/" + sid + "/subagents/workflows/wf_1c721b08-534/journal.jsonl")
	wfJournal2 := mustWrite(proj + "/" + sid + "/subagents/workflows/wf_99aa00bb-001/journal.jsonl")

	id := func(path string) string {
		h, err := PeekHeader(discover.File{Agent: "claude", Root: root, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		return h.SourceID
	}

	// The main file keeps its bare sessionId so a resume into the same file keeps
	// identity.
	if got := id(main); got != sid {
		t.Errorf("main source id = %q, want %q", got, sid)
	}

	// Subagents are grouped under the parent but distinct from it and each other.
	wantSubA := sid + "/subagents/agent-ac2d35a2e8e2aff8e"
	wantSubB := sid + "/subagents/agent-bb11ff00deadbeef0"
	if got := id(subA); got != wantSubA {
		t.Errorf("subagent A source id = %q, want %q", got, wantSubA)
	}
	if got := id(subB); got != wantSubB {
		t.Errorf("subagent B source id = %q, want %q", got, wantSubB)
	}

	// Two journal.jsonl files in different workflow dirs must not collide on the
	// bare "journal" basename.
	wantWf1 := sid + "/subagents/workflows/wf_1c721b08-534/journal"
	wantWf2 := sid + "/subagents/workflows/wf_99aa00bb-001/journal"
	if got := id(wfJournal1); got != wantWf1 {
		t.Errorf("workflow journal 1 source id = %q, want %q", got, wantWf1)
	}
	if got := id(wfJournal2); got != wantWf2 {
		t.Errorf("workflow journal 2 source id = %q, want %q", got, wantWf2)
	}

	all := []string{id(main), id(subA), id(subB), id(wfJournal1), id(wfJournal2)}
	seen := map[string]bool{}
	for _, v := range all {
		if seen[v] {
			t.Errorf("duplicate source id %q across distinct files", v)
		}
		seen[v] = true
		if v == sid && (v != id(main)) {
			t.Errorf("non-main file reused the parent sessionId %q", sid)
		}
	}
}

// TestSourceIDUnchangedForCodexAndPi pins that the fix is Claude-only: Codex and
// pi already carry one id per file, so their in-file id stands regardless of how
// deeply nested the file is.
func TestSourceIDUnchangedForCodexAndPi(t *testing.T) {
	root := t.TempDir()

	codexPath := filepath.Join(root, "2026", "06", "rollout-2026-06-27-x9.jsonl")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath,
		[]byte(`{"type":"session_meta","payload":{"id":"codex-id-9","cwd":"/home/ada/api","git":{"branch":"dev"}}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	piPath := filepath.Join(root, "encoded-cwd", "sessX.jsonl")
	if err := os.MkdirAll(filepath.Dir(piPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piPath, []byte(`{"type":"session","id":"pi-id-7","cwd":"/home/ada/proj"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hc, err := PeekHeader(discover.File{Agent: "codex", Root: root, Path: codexPath})
	if err != nil {
		t.Fatal(err)
	}
	if hc.SourceID != "codex-id-9" {
		t.Errorf("codex source id = %q, want codex-id-9", hc.SourceID)
	}

	hp, err := PeekHeader(discover.File{Agent: "pi", Root: root, Path: piPath})
	if err != nil {
		t.Fatal(err)
	}
	if hp.SourceID != "pi-id-7" {
		t.Errorf("pi source id = %q, want pi-id-7", hp.SourceID)
	}
}

// fakeGit returns canned responses keyed by a short name for the git subcommand
// ("rev-parse" or "remote get-url"), independent of the trailing flags.
func fakeGit(responses map[string]string, errs map[string]error) GitRunner {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		key := args[0]
		if args[0] == "remote" {
			key = "remote get-url"
		}
		if err, ok := errs[key]; ok {
			return "", err
		}
		return responses[key], nil
	}
}

func TestResolveSuccess(t *testing.T) {
	cwd := t.TempDir() // a directory that exists on disk
	file := writeFile(t, cwd, "sess.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":%q,"gitBranch":"main","sessionId":"s1"}`+"\n", cwd))

	r := NewWith(fakeGit(map[string]string{
		"rev-parse":      "true",
		"remote get-url": "git@github.com:owner/repo.git",
	}, nil), nil)

	res := r.Resolve(context.Background(), discover.File{Agent: "claude", Root: cwd, Path: file})
	if res.Skipped {
		t.Fatalf("unexpected skip: %s", res.Reason)
	}
	if res.Kind != KindRemote {
		t.Errorf("kind = %q, want remote", res.Kind)
	}
	if res.ProjectKey != "github.com/owner/repo" {
		t.Errorf("project key = %q", res.ProjectKey)
	}
	if res.Header.GitBranch != "main" || res.Header.SourceID != "s1" {
		t.Errorf("header = %+v", res.Header)
	}
}

// TestResolveClassifies confirms that the formerly-skipped cases are now backed
// up with a kind: a missing or unknown working directory is orphaned, and a
// directory with no usable git remote is standalone. None are skipped, and only
// remote results carry a project key.
func TestResolveClassifies(t *testing.T) {
	existing := t.TempDir()

	cases := []struct {
		name       string
		cwd        string
		git        GitRunner
		wantKind   Kind
		wantReason string // substring
	}{
		{
			name:       "no cwd is orphaned",
			cwd:        "",
			git:        fakeGit(nil, nil),
			wantKind:   KindOrphaned,
			wantReason: "no working directory recorded",
		},
		{
			name:       "missing cwd is orphaned",
			cwd:        filepath.Join(existing, "gone"),
			git:        fakeGit(nil, nil),
			wantKind:   KindOrphaned,
			wantReason: "cwd no longer exists",
		},
		{
			name:       "not a git repo is standalone",
			cwd:        existing,
			git:        fakeGit(nil, map[string]error{"rev-parse": fmt.Errorf("fatal: not a git repo")}),
			wantKind:   KindStandalone,
			wantReason: "is not a git repository",
		},
		{
			name:       "no origin is standalone",
			cwd:        existing,
			git:        fakeGit(map[string]string{"rev-parse": "true"}, map[string]error{"remote get-url": fmt.Errorf("no such remote")}),
			wantKind:   KindStandalone,
			wantReason: "has no origin remote",
		},
		{
			name: "multiple origin urls is standalone",
			cwd:  existing,
			git: fakeGit(map[string]string{
				"rev-parse":      "true",
				"remote get-url": "git@github.com:owner/repo.git\nhttps://github.com/owner/repo.git",
			}, nil),
			wantKind:   KindStandalone,
			wantReason: "origin has multiple URLs",
		},
		{
			name: "unrecognized origin url is standalone",
			cwd:  existing,
			git: fakeGit(map[string]string{
				"rev-parse":      "true",
				"remote get-url": "not-a-url",
			}, nil),
			wantKind:   KindStandalone,
			wantReason: "origin URL is unrecognized",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			file := writeFile(t, dir, "sess.jsonl",
				fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", c.cwd))
			r := NewWith(c.git, nil)
			res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
			if res.Skipped {
				t.Fatalf("unexpected skip: %s", res.Reason)
			}
			if res.Kind != c.wantKind {
				t.Errorf("kind = %q, want %q", res.Kind, c.wantKind)
			}
			if res.ProjectKey != "" {
				t.Errorf("non-remote result carried project key %q", res.ProjectKey)
			}
			if !strings.Contains(res.Reason, c.wantReason) {
				t.Errorf("reason = %q, want substring %q", res.Reason, c.wantReason)
			}
		})
	}
}

func TestResolveCachesPerDirectory(t *testing.T) {
	cwd := t.TempDir()
	file := writeFile(t, cwd, "sess.jsonl",
		fmt.Sprintf(`{"type":"user","cwd":%q}`+"\n", cwd))

	calls := 0
	git := func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if args[0] == "rev-parse" {
			return "true", nil
		}
		return "git@github.com:owner/repo.git", nil
	}
	r := NewWith(git, nil)

	for i := 0; i < 3; i++ {
		res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
		if res.Skipped {
			t.Fatalf("unexpected skip: %s", res.Reason)
		}
	}
	// Two git calls for the first resolve; the rest are served from cache.
	if calls != 2 {
		t.Errorf("git calls = %d, want 2 (cached after first)", calls)
	}
}
