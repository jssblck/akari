package resolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// claudeLine renders a realistic Claude transcript entry for a cwd: a typed user
// record carrying a message, which is the shape resolve's positive session
// signature requires. Extra top-level fields (gitBranch, sessionId) may be
// appended for tests that assert on them.
func claudeLine(cwd string, extra ...string) string {
	fields := fmt.Sprintf(`"type":"user","cwd":%q,"message":{"content":"hi"}`, cwd)
	for _, x := range extra {
		fields += "," + x
	}
	return "{" + fields + "}\n"
}

func TestPeekHeader(t *testing.T) {
	dir := t.TempDir()

	// A Claude main file is named by its session id, so its path-derived source id
	// equals that id.
	claude := writeFile(t, dir, "c-123.jsonl",
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

// TestOpenSameFileRejectsMismatchedIdentity proves the SameFile plumbing PeekHeader
// relies on to close its read-time TOCTOU window (see PeekHeader's doc comment),
// without needing an actual race: it hands openSameFile a real Lstat of a
// different file than the one at path, which is exactly what a caller sees when
// the path was swapped between its own Lstat and this Open. This runs on every
// platform, unlike the symlink-swap scenario in peek_special_unix_test.go, which
// needs symlinks to simulate realistically.
func TestOpenSameFileRejectsMismatchedIdentity(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.jsonl", "a\n")
	b := writeFile(t, dir, "b.jsonl", "b\n")

	lstA, err := os.Lstat(a)
	if err != nil {
		t.Fatal(err)
	}
	lstB, err := os.Lstat(b)
	if err != nil {
		t.Fatal(err)
	}

	// Matching identity: opening a against its own Lstat succeeds.
	f, err := openSameFile(a, lstA)
	if err != nil {
		t.Fatalf("openSameFile with matching identity: %v", err)
	}
	f.Close()

	// Mismatched identity: opening a against b's Lstat is exactly what a caller
	// would see if a had been swapped for a different file in between, and must
	// be rejected rather than silently returning a's content under b's identity.
	if _, err := openSameFile(a, lstB); err == nil {
		t.Fatal("openSameFile accepted a file that does not match the given Lstat")
	}
}

// TestPeekHeaderRejectsNonSession is the positive-detection guard: a parseable
// *.jsonl that is not a session for its agent (a tool-output log, an event feed
// under a custom extra_root) must return errNotSession rather than a header, even
// when it happens to carry a cwd field. This is what keeps discovery's suffix
// match from ingesting arbitrary JSONL as junk sessions.
func TestPeekHeaderRejectsNonSession(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		agent, name, content string
	}{
		// A log line with a cwd but no session shape: the case that formerly resolved
		// as a standalone/orphaned junk session.
		{"claude", "log1.jsonl", `{"level":"info","msg":"built","cwd":"/home/grace/app"}` + "\n"},
		// A Claude "type" that is not a transcript entry, and one that lacks a message.
		{"claude", "log2.jsonl", `{"type":"user","cwd":"/home/grace/app"}` + "\n"},
		{"claude", "log3.jsonl", `{"type":"event","payload":{"cwd":"/x"}}` + "\n"},
		// Codex without the payload wrapper it always writes.
		{"codex", "rollout-x.jsonl", `{"type":"log","cwd":"/home/ada/api"}` + "\n"},
		// pi lines that are neither a session header (no id/cwd) nor a typed message.
		{"pi", "n1.jsonl", `{"type":"session"}` + "\n"},
		{"pi", "n2.jsonl", `{"type":"tool","output":"hello","cwd":"/x"}` + "\n"},
		// Valid JSON that carries no recognizable shape at all.
		{"claude", "n3.jsonl", `{"hello":"world"}` + "\n{}\n"},
	}
	for _, c := range cases {
		path := writeFile(t, dir, c.name, c.content)
		_, err := PeekHeader(discover.File{Agent: c.agent, Root: dir, Path: path})
		if !errors.Is(err, errNotSession) {
			t.Errorf("%s/%s: err = %v, want errNotSession", c.agent, c.name, err)
		}
	}
}

// TestPeekHeaderAcceptsRealSessions pins the other side: a minimal but genuine
// header for each agent passes the signature and yields the expected cwd, so the
// positive test never rejects a real session.
func TestPeekHeaderAcceptsRealSessions(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		agent, name, content, wantCwd string
	}{
		{"claude", "c.jsonl", `{"type":"assistant","cwd":"/a","message":{"content":[]}}` + "\n", "/a"},
		{"codex", "rollout-c.jsonl", `{"type":"session_meta","payload":{"cwd":"/b"}}` + "\n", "/b"},
		{"pi", "p.jsonl", `{"type":"session","id":"p-1","cwd":"/c"}` + "\n", "/c"},
		// A pi file that opens with a typed message line (a tail with no header) still
		// reads as a session; it just has no cwd, so it resolves as orphaned later.
		{"pi", "p2.jsonl", `{"type":"message","message":{"role":"user","content":"hi"}}` + "\n", ""},
	}
	for _, c := range cases {
		path := writeFile(t, dir, c.name, c.content)
		h, err := PeekHeader(discover.File{Agent: c.agent, Root: dir, Path: path})
		if err != nil {
			t.Errorf("%s/%s: unexpected err %v", c.agent, c.name, err)
			continue
		}
		if h.Cwd != c.wantCwd {
			t.Errorf("%s/%s: cwd = %q, want %q", c.agent, c.name, h.Cwd, c.wantCwd)
		}
	}
}

// TestResolveSkipsNonSession confirms the not-a-session verdict flows through
// Resolve as a Skipped result with a clear, agent-named reason (not an upload as a
// standalone/orphaned session), which is what the sync summary and watch log show.
func TestResolveSkipsNonSession(t *testing.T) {
	dir := t.TempDir()
	// A noisy JSONL with a cwd that points at a real directory: without positive
	// detection it would resolve all the way to a standalone/remote junk session.
	path := writeFile(t, dir, "events.jsonl",
		fmt.Sprintf(`{"level":"info","event":"tick","cwd":%q}`+"\n", dir))
	r := NewWith(fakeGit(map[string]string{"rev-parse": "true",
		"remote get-url": "git@github.com:owner/repo.git"}, nil), nil)
	res := r.Resolve(context.Background(), discover.File{Agent: "claude", Root: dir, Path: path})
	if !res.Skipped {
		t.Fatalf("kind = %q, want skipped", res.Kind)
	}
	if res.ProjectKey != "" {
		t.Errorf("skipped result carried project key %q", res.ProjectKey)
	}
	if !strings.Contains(res.Reason, "not a claude session") {
		t.Errorf("reason = %q, want it to name the agent and the not-a-session cause", res.Reason)
	}
}

func TestResolveReportsHeaderReadFailure(t *testing.T) {
	t.Parallel()
	missing := discover.File{Agent: "claude", Path: filepath.Join(t.TempDir(), "missing.jsonl")}
	res := NewWith(nil, nil).Resolve(context.Background(), missing)
	if res.Err == nil {
		t.Fatal("Resolve counted a missing session file as a successful result")
	}
	if res.Skipped {
		t.Fatal("Resolve classified an I/O failure as a non-session skip")
	}
}

// TestClaudeSourceIDUnique is the regression guard for the source-id collision:
// a Claude main session file and every subagent and workflow file beneath it all
// record the same in-file sessionId, so before the fix they folded onto one
// server row and clobbered each other. Each file must now resolve to a distinct
// path-derived source id, with subagents kept grouped under their parent. An
// ordinary main file is named by its session id, so it still resolves to that id.
func TestClaudeSourceIDUnique(t *testing.T) {
	root := t.TempDir()
	const sid = "4a7929e8-5b80-48e6-8ccc-a8919c89cd6d"

	// All four files carry the parent sessionId in their first line, exactly as
	// Claude writes subagent and workflow files.
	line := fmt.Sprintf(`{"type":"user","cwd":"/home/ada/app","gitBranch":"main","sessionId":%q,"message":{"content":"hi"}}`+"\n", sid)

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

	// The main file is named by its session id, so its path-derived id is that id;
	// a resume that appends to the same file keeps identity.
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

// TestClaudeForkedSessionDistinct guards the second half of the collision: a
// resumed or forked Claude session writes a new file (named by a fresh id) that
// still records the ORIGINAL session's sessionId inside. Keyed on the in-file
// sessionId the two main files would fold onto one row; keyed on the file name
// they stay distinct, so both are backed up losslessly.
func TestClaudeForkedSessionDistinct(t *testing.T) {
	root := t.TempDir()
	const parent = "10c63fb7-6a1f-48d8-95f4-51bfd15c57c2"
	const fork = "c0ce02b7-4f3e-49ff-9a3d-6784654cdfaa"
	proj := "-home-ada-app"

	// Both files carry the parent's sessionId in their first line; only their file
	// names differ.
	line := fmt.Sprintf(`{"type":"user","cwd":"/home/ada/app","sessionId":%q,"message":{"content":"hi"}}`+"\n", parent)
	mustWrite := func(name string) string {
		path := filepath.Join(root, proj, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	id := func(path string) string {
		h, err := PeekHeader(discover.File{Agent: "claude", Root: root, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		return h.SourceID
	}

	parentID := id(mustWrite(parent + ".jsonl"))
	forkID := id(mustWrite(fork + ".jsonl"))
	if parentID != parent {
		t.Errorf("parent source id = %q, want %q", parentID, parent)
	}
	if forkID != fork {
		t.Errorf("fork source id = %q, want %q (must not reuse the in-file sessionId)", forkID, fork)
	}
	if parentID == forkID {
		t.Errorf("forked session collided with its parent on id %q", parentID)
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
// ("rev-parse", "remote get-url", or "git-common-dir"). The common-dir lookup
// shares the "rev-parse" verb, so it is matched on its flag.
func fakeGit(responses map[string]string, errs map[string]error) GitRunner {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		key := args[0]
		switch {
		case args[0] == "remote":
			key = "remote get-url"
		case hasArg(args, "--git-common-dir"):
			key = "git-common-dir"
		}
		if err, ok := errs[key]; ok {
			return "", err
		}
		return responses[key], nil
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestResolveSuccess(t *testing.T) {
	cwd := t.TempDir() // a directory that exists on disk
	file := writeFile(t, cwd, "s1.jsonl", claudeLine(cwd, `"gitBranch":"main"`, `"sessionId":"s1"`))

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
			file := writeFile(t, dir, "sess.jsonl", claudeLine(c.cwd))
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

// TestResolveStandaloneGroupsByCommonDir confirms that a standalone session in a
// live git worktree (a work tree with no usable origin) reports the main worktree
// as its LocalRoot, derived from git's common dir. Two different worktrees of the
// same local-only repo must resolve to the same root so the server can collapse
// them into one project, the way a canonical remote collapses a repo's worktrees.
func TestResolveStandaloneGroupsByCommonDir(t *testing.T) {
	main := t.TempDir()
	common := filepath.Join(main, ".git")
	git := fakeGit(map[string]string{
		"rev-parse":      "true",
		"git-common-dir": common,
	}, map[string]error{
		"remote get-url": fmt.Errorf("no such remote"),
	})

	for _, wt := range []string{"feature-a", "feature-b"} {
		dir := t.TempDir()
		file := writeFile(t, dir, "sess.jsonl",
			claudeLine(dir))
		r := NewWith(git, nil)
		res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
		if res.Kind != KindStandalone {
			t.Fatalf("%s: kind = %q, want standalone", wt, res.Kind)
		}
		if res.LocalRoot != main {
			t.Errorf("%s: local root = %q, want %q (the shared main worktree)", wt, res.LocalRoot, main)
		}
	}
}

// TestResolveStandaloneLocalRootAcrossBranches confirms every no-remote branch
// (origin lookup error, empty output, multiple URLs, unrecognized URL) attaches
// the shared root, not just the get-url error path: each is a real local-only
// repo whose worktrees should collapse.
func TestResolveStandaloneLocalRootAcrossBranches(t *testing.T) {
	mainWT := t.TempDir()
	common := filepath.Join(mainWT, ".git")
	cases := []struct {
		name      string
		responses map[string]string
		errs      map[string]error
	}{
		{
			name:      "origin lookup errors",
			responses: map[string]string{"rev-parse": "true", "git-common-dir": common},
			errs:      map[string]error{"remote get-url": fmt.Errorf("no such remote")},
		},
		{
			name:      "empty origin output",
			responses: map[string]string{"rev-parse": "true", "git-common-dir": common, "remote get-url": ""},
		},
		{
			name: "multiple origin urls",
			responses: map[string]string{"rev-parse": "true", "git-common-dir": common,
				"remote get-url": "git@github.com:o/r.git\nhttps://github.com/o/r.git"},
		},
		{
			name:      "unrecognized origin url",
			responses: map[string]string{"rev-parse": "true", "git-common-dir": common, "remote get-url": "not-a-url"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			file := writeFile(t, dir, "sess.jsonl",
				claudeLine(dir))
			r := NewWith(fakeGit(c.responses, c.errs), nil)
			res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
			if res.Kind != KindStandalone {
				t.Fatalf("kind = %q, want standalone", res.Kind)
			}
			if res.LocalRoot != mainWT {
				t.Errorf("local root = %q, want %q", res.LocalRoot, mainWT)
			}
		})
	}
}

// TestResolveLocalRootBareRepoPreservesCommonDir covers the branch where the
// common dir is not "<worktree>/.git" (a bare repo, or a separated git dir): with
// no main worktree to point at, the common dir itself stands as the shared key.
func TestResolveLocalRootBareRepoPreservesCommonDir(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "myrepo.git")
	dir := t.TempDir()
	file := writeFile(t, dir, "sess.jsonl",
		claudeLine(dir))
	r := NewWith(fakeGit(map[string]string{"rev-parse": "true", "git-common-dir": bare},
		map[string]error{"remote get-url": fmt.Errorf("no such remote")}), nil)
	res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
	if res.LocalRoot != bare {
		t.Errorf("local root = %q, want %q (bare common dir preserved)", res.LocalRoot, bare)
	}
}

// TestResolveStandaloneNoRootWhenCommonDirUnavailable pins the best-effort
// fallback: when git cannot report the common dir, the session is still
// standalone but carries no LocalRoot, so the server keys it on its own cwd.
func TestResolveStandaloneNoRootWhenCommonDirUnavailable(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, dir, "sess.jsonl",
		claudeLine(dir))
	r := NewWith(fakeGit(map[string]string{"rev-parse": "true"}, map[string]error{
		"remote get-url": fmt.Errorf("no such remote"),
		"git-common-dir": fmt.Errorf("unsupported"),
	}), nil)
	res := r.Resolve(context.Background(), discover.File{Agent: "claude", Path: file})
	if res.Kind != KindStandalone {
		t.Fatalf("kind = %q, want standalone", res.Kind)
	}
	if res.LocalRoot != "" {
		t.Errorf("local root = %q, want empty", res.LocalRoot)
	}
}

// TestResolveRealWorktreeGroupsByCommonDir exercises the real system git against
// an actual local-only repo with two worktrees. The fakeGit tests cannot catch
// platform path quirks (git reports forward slashes, an absolute common dir from
// a linked worktree but a relative one from the main checkout); this proves the
// normalization in localRoot collapses the main checkout and both worktrees onto
// one identical LocalRoot. It skips where git is unavailable.
func TestResolveRealWorktreeGroupsByCommonDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
			"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	base := t.TempDir()
	main := filepath.Join(base, "repo") // a local-only repo: no origin remote
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	run(main, "init", "-b", "main")
	writeFile(t, main, "READY", "x")
	run(main, "add", "-A")
	run(main, "commit", "-m", "init")

	wtA := filepath.Join(base, "wt-a")
	wtB := filepath.Join(base, "wt-b")
	run(main, "worktree", "add", "-b", "feature-a", wtA)
	run(main, "worktree", "add", "-b", "feature-b", wtB)

	// Resolve a session whose cwd is each of the three checkouts.
	rootFor := func(cwd string) Result {
		file := writeFile(t, cwd, "sess.jsonl",
			claudeLine(cwd))
		return New().Resolve(ctx, discover.File{Agent: "claude", Path: file})
	}
	rMain := rootFor(main)
	rA := rootFor(wtA)
	rB := rootFor(wtB)

	for name, res := range map[string]Result{"main": rMain, "wt-a": rA, "wt-b": rB} {
		if res.Kind != KindStandalone {
			t.Fatalf("%s: kind = %q, want standalone (no remote)", name, res.Kind)
		}
		if res.LocalRoot == "" {
			t.Fatalf("%s: empty local root", name)
		}
	}
	// The collapse invariant: every checkout of the repo reports the same root.
	if rMain.LocalRoot != rA.LocalRoot || rA.LocalRoot != rB.LocalRoot {
		t.Errorf("worktrees did not collapse: main=%q wt-a=%q wt-b=%q",
			rMain.LocalRoot, rA.LocalRoot, rB.LocalRoot)
	}
	// And it is the main worktree, not a per-worktree path or a .git dir.
	if want, err := filepath.Abs(main); err != nil {
		t.Fatal(err)
	} else if filepath.Clean(rMain.LocalRoot) != filepath.Clean(want) {
		t.Errorf("local root = %q, want the main worktree %q", rMain.LocalRoot, want)
	}
}

func TestResolveCachesPerDirectory(t *testing.T) {
	cwd := t.TempDir()
	file := writeFile(t, cwd, "sess.jsonl",
		claudeLine(cwd))

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
	// The first resolve checks the repo, origin, and config path; later resolves
	// validate the cached config fingerprint without starting git.
	if calls != 3 {
		t.Errorf("git calls = %d, want 3 (cached after first)", calls)
	}
}
