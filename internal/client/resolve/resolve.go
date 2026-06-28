// Package resolve turns a discovered session file into the project it belongs to.
// It peeks the file header for the working directory, then resolves that
// directory's git origin remote to a canonical project key. Either hop can fail;
// rather than dropping the session, a failure classifies it: a folder with no
// usable git remote is standalone, a folder that no longer exists on disk is
// orphaned. Only a file we cannot even read a header from is skipped.
package resolve

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/gitremote"
	"github.com/tidwall/gjson"
)

// Header is the minimum the client reads from a session file before deciding
// where it belongs. The full parse is the server's job.
type Header struct {
	Cwd       string
	GitBranch string
	SourceID  string
}

// Kind classifies how a session resolves to a project.
type Kind string

const (
	// KindRemote is a session whose working directory resolves to a canonical git
	// remote. ProjectKey holds that remote.
	KindRemote Kind = "remote"
	// KindStandalone is a session whose working directory exists but yields no
	// usable git remote: not a repository, no origin, multiple origin URLs, or an
	// unrecognized origin URL. It is backed up and keyed by its local location.
	KindStandalone Kind = "standalone"
	// KindOrphaned is a session whose working directory is unknown or no longer
	// exists on disk, so its remote can never be resolved. It is still backed up.
	KindOrphaned Kind = "orphaned"
)

// Result is the outcome of resolving one file. Kind classifies the session;
// ProjectKey is the canonical remote only when Kind is KindRemote. Reason carries
// the human-readable detail behind a standalone or orphaned classification (and
// the failure detail when Skipped). Skipped is true only when the file's header
// could not be read at all, leaving nothing to upload.
type Result struct {
	File       discover.File
	Header     Header
	Kind       Kind
	ProjectKey string
	Skipped    bool
	Reason     string
}

// GitRunner runs a git subcommand in dir and returns its trimmed stdout. The
// default shells out to the system git; tests inject their own.
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// Resolver resolves files to projects, caching git lookups per directory for the
// process lifetime (the client keeps no on-disk state).
type Resolver struct {
	aliases map[string]string
	git     GitRunner
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	key    string
	reason string // non-empty means this directory resolves to a skip
}

// New builds a Resolver with the default system-git runner and ssh alias map.
func New() *Resolver {
	return &Resolver{
		aliases: gitremote.LoadSSHAliases(),
		git:     systemGit,
		timeout: 5 * time.Second,
		cache:   map[string]cacheEntry{},
	}
}

// NewWith builds a Resolver with an explicit git runner and alias map, for tests.
func NewWith(git GitRunner, aliases map[string]string) *Resolver {
	return &Resolver{
		aliases: aliases,
		git:     git,
		timeout: 5 * time.Second,
		cache:   map[string]cacheEntry{},
	}
}

// Resolve peeks the file header and classifies the session. A session with a
// resolvable git remote is KindRemote; one whose folder exists but has no usable
// remote is KindStandalone; one whose folder is unknown or gone is KindOrphaned.
// All three are returned ready to upload. Only a file whose header cannot be read
// is Skipped, since without a header there is nothing to identify or send.
func (r *Resolver) Resolve(ctx context.Context, f discover.File) Result {
	h, err := PeekHeader(f.Agent, f.Path)
	if err != nil {
		return Result{File: f, Skipped: true, Reason: "could not read header: " + err.Error()}
	}
	res := Result{File: f, Header: h}

	if h.Cwd == "" {
		res.Kind, res.Reason = KindOrphaned, "no working directory recorded"
		return res
	}
	if info, err := os.Stat(h.Cwd); err != nil || !info.IsDir() {
		res.Kind, res.Reason = KindOrphaned, "cwd no longer exists: "+h.Cwd
		return res
	}

	key, reason := r.project(ctx, h.Cwd)
	if reason != "" {
		res.Kind, res.Reason = KindStandalone, reason
		return res
	}
	res.Kind, res.ProjectKey = KindRemote, key
	return res
}

// project resolves a working directory to a canonical project key, caching both
// successes and skips. The returned reason is non-empty exactly when the key is
// empty.
func (r *Resolver) project(ctx context.Context, cwd string) (key, reason string) {
	r.mu.Lock()
	if e, ok := r.cache[cwd]; ok {
		r.mu.Unlock()
		return e.key, e.reason
	}
	r.mu.Unlock()

	key, reason = r.resolveGit(ctx, cwd)

	r.mu.Lock()
	r.cache[cwd] = cacheEntry{key: key, reason: reason}
	r.mu.Unlock()
	return key, reason
}

func (r *Resolver) resolveGit(ctx context.Context, cwd string) (key, reason string) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.git(ctx, cwd, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", cwd + " is not a git repository"
	}
	out, err := r.git(ctx, cwd, "remote", "get-url", "--all", "origin")
	if err != nil {
		return "", cwd + " has no origin remote"
	}
	urls := nonEmptyLines(out)
	switch {
	case len(urls) == 0:
		return "", cwd + " has no origin remote"
	case len(urls) > 1:
		return "", cwd + " origin has multiple URLs"
	}
	remote, err := gitremote.Canonicalize(urls[0], r.aliases)
	if err != nil {
		return "", cwd + " origin URL is unrecognized: " + err.Error()
	}
	return remote.Key, ""
}

// PeekHeader reads only as much of the file as it needs to extract cwd, the git
// branch, and a source session id for the given agent.
func PeekHeader(agent, path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer f.Close()

	var h Header
	h.SourceID = sourceIDFromName(agent, path)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	const maxLines = 500 // cwd appears early in every format; cap the peek
	for i := 0; sc.Scan() && i < maxLines; i++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !gjson.Valid(line) {
			continue
		}
		applyHeaderLine(agent, gjson.Parse(line), &h)
		if h.Cwd != "" {
			break // cwd is the field that gates resolution; stop once we have it
		}
	}
	if err := sc.Err(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// applyHeaderLine pulls header fields out of one parsed line for the agent.
func applyHeaderLine(agent string, e gjson.Result, h *Header) {
	switch agent {
	case "claude":
		if v := e.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := e.Get("gitBranch").String(); v != "" {
			h.GitBranch = v
		}
		if v := e.Get("sessionId").String(); v != "" {
			h.SourceID = v
		}
	case "codex":
		p := e.Get("payload")
		if v := p.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := p.Get("git.branch").String(); v != "" {
			h.GitBranch = v
		}
		if v := p.Get("id").String(); v != "" {
			h.SourceID = v
		}
	case "pi":
		if v := e.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := e.Get("id").String(); v != "" {
			h.SourceID = v
		}
	}
}

// sourceIDFromName derives a stable source id from the filename, used as a
// fallback when the header carries no id. The full stem is stable and unique per
// file for all three agents (including Codex's rollout-<timestamp>-<uuid>).
func sourceIDFromName(_ string, path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// systemGit runs the real git binary with the context's deadline. A non-zero
// exit (for example "not a git repository" or a missing origin) surfaces as an
// error, which the caller maps to a specific skip reason.
func systemGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
