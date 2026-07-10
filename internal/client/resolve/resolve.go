// Package resolve turns a discovered session file into the project it belongs to.
// It peeks the file header for the working directory, then resolves that
// directory's git origin remote to a canonical project key. Either hop can fail;
// rather than dropping the session, a failure classifies it: a folder with no
// usable git remote is standalone, a folder that no longer exists on disk is
// orphaned. A file is skipped only when it cannot be read at all or when its
// header carries no recognizable session signature, so an arbitrary *.jsonl that
// merely shares the suffix (a tool-output log, an event feed) under a custom
// extra_root is rejected rather than ingested as a junk session.
package resolve

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/gitremote"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

// errNotSession marks a file whose header could be read but carries no
// recognizable session signature for its agent: the positive test that a
// discovered *.jsonl actually is a session. Resolve maps it to a Skipped result
// with a clear reason, distinct from an unreadable file. See sessionSignature.
var errNotSession = errors.New("no recognizable session header")

// Header is the minimum the client reads from a session file before deciding
// where it belongs. The full parse is the server's job.
type Header struct {
	Cwd       string
	GitBranch string
	SourceID  string

	// sessionID holds the raw in-file session id for the agents that key on it
	// (Codex's payload.id, pi's id). Claude does not use it: its sessionId is
	// repeated across a session's subagent and workflow files and is even reused by
	// a resumed or forked session in a new file, so Claude keys on the file's path
	// instead. PeekHeader turns this into the final SourceID.
	sessionID string
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
// a positive non-session verdict). Skipped is true only when readable content is
// not a session for its declared agent. Err carries I/O and structural read
// failures so a backup command cannot silently count an unreadable session as a
// successful skip.
//
// LocalRoot is set only for a standalone session whose folder is a live git
// worktree: it holds the main worktree shared by every worktree of the repo, so
// the server can collapse a local-only repo's worktrees into one project the same
// way a canonical remote collapses a remote-backed repo's. It is empty for remote
// sessions, for orphaned sessions (the worktree is gone, so git cannot report
// it), and for non-git standalone folders.
type Result struct {
	File       discover.File
	Header     Header
	Kind       Kind
	ProjectKey string
	LocalRoot  string
	Skipped    bool
	Reason     string
	Err        error
}

// GitRunner runs a git subcommand in dir and returns its trimmed stdout. The
// default shells out to the system git; tests inject their own.
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// Resolver resolves files to projects. Its bounded in-memory cache avoids repeat
// Git processes, invalidates direct repository config changes immediately, and
// caps the age of results that depend on other Git config sources.
type Resolver struct {
	aliases map[string]string
	git     GitRunner
	timeout time.Duration
	now     func() time.Time
	stat    func(string) (os.FileInfo, error)

	mu               sync.Mutex
	cache            map[string]cacheEntry
	cacheUse         uint64
	cacheVersion     uint64
	cacheLimit       int
	cacheFallbackTTL time.Duration
	lookups          singleflight.Group
}

type cacheEntry struct {
	key                  string
	root                 string // the local project root for a no-remote worktree; empty otherwise
	reason               string // non-empty means this directory resolves as standalone
	config               configFingerprint
	cachedAt             time.Time
	fingerprintFailureAt time.Time
	lastUsed             uint64
	version              uint64
}

type configFingerprint struct {
	path string
	info os.FileInfo
}

type projectResult struct {
	key    string
	root   string
	reason string
}

type cacheEntryCheck uint8

const (
	cacheEntryValid cacheEntryCheck = iota
	cacheEntryExpired
	cacheEntryChanged
	cacheEntryStatFailed
)

const (
	defaultCacheLimit       = 1024
	defaultCacheFallbackTTL = 5 * time.Minute
)

// New builds a Resolver with the default system-git runner and ssh alias map.
func New() *Resolver {
	return newResolver(systemGit, gitremote.LoadSSHAliases())
}

// NewWith builds a Resolver with an explicit git runner and alias map, for tests.
func NewWith(git GitRunner, aliases map[string]string) *Resolver {
	return newResolver(git, aliases)
}

func newResolver(git GitRunner, aliases map[string]string) *Resolver {
	return &Resolver{
		aliases:          aliases,
		git:              git,
		timeout:          5 * time.Second,
		now:              time.Now,
		stat:             os.Stat,
		cache:            map[string]cacheEntry{},
		cacheLimit:       defaultCacheLimit,
		cacheFallbackTTL: defaultCacheFallbackTTL,
	}
}

// Resolve peeks the file header and classifies the session. A session with a
// resolvable git remote is KindRemote; one whose folder exists but has no usable
// remote is KindStandalone; one whose folder is unknown or gone is KindOrphaned.
// All three are returned ready to upload. Only a file whose header cannot be read
// is Skipped, since without a header there is nothing to identify or send.
func (r *Resolver) Resolve(ctx context.Context, f discover.File) Result {
	h, err := PeekHeader(f)
	if err != nil {
		if errors.Is(err, errNotSession) {
			return Result{File: f, Skipped: true, Reason: "not a " + f.Agent + " session: " + err.Error()}
		}
		return Result{File: f, Err: fmt.Errorf("read session header: %w", err)}
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

	key, root, reason := r.project(ctx, h.Cwd)
	if reason != "" {
		res.Kind, res.Reason, res.LocalRoot = KindStandalone, reason, root
		return res
	}
	res.Kind, res.ProjectKey = KindRemote, key
	return res
}

// project resolves a working directory to a canonical project key, caching both
// successes and skips. The returned reason is non-empty exactly when the key is
// empty; root is the local project root for a no-remote worktree (empty
// otherwise).
func (r *Resolver) project(ctx context.Context, cwd string) (key, root, reason string) {
	if cached, ok := r.cachedProject(cwd, r.now()); ok {
		return cached.key, cached.root, cached.reason
	}

	result := r.lookupProject(ctx, cwd)
	select {
	case <-ctx.Done():
		return "", "", cwd + " project lookup canceled: " + ctx.Err().Error()
	case completed := <-result:
		if completed.Err != nil {
			return "", "", cwd + " project lookup failed: " + completed.Err.Error()
		}
		resolved, ok := completed.Val.(projectResult)
		if !ok {
			return "", "", cwd + " project lookup returned an invalid result"
		}
		return resolved.key, resolved.root, resolved.reason
	}
}

func (r *Resolver) lookupProject(ctx context.Context, cwd string) <-chan singleflight.Result {
	return r.lookups.DoChan(cwd, func() (any, error) {
		// A follower can miss just before the leader fills the cache. Rechecking
		// inside the flight avoids a second Git lookup in that handoff window.
		if cached, ok := r.cachedProject(cwd, r.now()); ok {
			return cached, nil
		}

		// The lookup belongs to the resolver rather than the first caller. One
		// canceled waiter must not abort work shared by other files, but the fixed
		// timeout still bounds a Git process after every waiter has left.
		lookupStarted := r.now()
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
		defer cancel()

		key, root, reason, config := r.resolveGit(lookupCtx, cwd)
		resolved := projectResult{key: key, root: root, reason: reason}
		return r.storeProject(cwd, resolved, config, lookupStarted), nil
	})
}

func (r *Resolver) cachedProject(cwd string, now time.Time) (projectResult, bool) {
	for {
		r.mu.Lock()
		e, ok := r.cache[cwd]
		if !ok {
			r.mu.Unlock()
			return projectResult{}, false
		}
		r.cacheUse++
		e.lastUsed = r.cacheUse
		r.cache[cwd] = e
		r.mu.Unlock()

		// A config path may be on a slow network filesystem. Probe it without the
		// global cache lock so unrelated repositories can still resolve.
		check := r.checkCacheEntry(e, now)

		r.mu.Lock()
		current, ok := r.cache[cwd]
		if !ok {
			r.mu.Unlock()
			return projectResult{}, false
		}
		if current.version != e.version {
			r.mu.Unlock()
			now = r.now()
			continue
		}
		now = r.now()
		if !now.Before(current.cachedAt.Add(r.cacheFallbackTTL)) {
			delete(r.cache, cwd)
			r.mu.Unlock()
			return projectResult{}, false
		}

		switch check {
		case cacheEntryExpired, cacheEntryChanged:
			delete(r.cache, cwd)
			r.mu.Unlock()
			return projectResult{}, false
		case cacheEntryStatFailed:
			if current.fingerprintFailureAt.IsZero() {
				current.fingerprintFailureAt = now
			}
			if !now.Before(current.fingerprintFailureAt.Add(r.cacheFallbackTTL)) {
				delete(r.cache, cwd)
				r.mu.Unlock()
				return projectResult{}, false
			}
		case cacheEntryValid:
			current.fingerprintFailureAt = time.Time{}
		}
		r.cache[cwd] = current
		resolved := projectResult{key: current.key, root: current.root, reason: current.reason}
		r.mu.Unlock()
		return resolved, true
	}
}

func (r *Resolver) storeProject(cwd string, resolved projectResult, config configFingerprint, now time.Time) projectResult {
	e := cacheEntry{key: resolved.key, root: resolved.root, reason: resolved.reason, config: config, cachedAt: now}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Same-key lookups are serialized by singleflight. Keep this guard so a
	// future alternate cache writer cannot let an older lookup replace newer
	// state.
	if current, exists := r.cache[cwd]; exists && current.cachedAt.After(e.cachedAt) {
		return projectResult{key: current.key, root: current.root, reason: current.reason}
	}
	r.cacheUse++
	e.lastUsed = r.cacheUse
	r.cacheVersion++
	e.version = r.cacheVersion
	if _, exists := r.cache[cwd]; !exists && len(r.cache) >= r.cacheLimit {
		r.evictLeastRecentlyUsed()
	}
	r.cache[cwd] = e
	return resolved
}

func (r *Resolver) checkCacheEntry(e cacheEntry, now time.Time) cacheEntryCheck {
	// Git may read included, worktree, global, or command-scoped config that the
	// repository config fingerprint cannot observe. The maximum age bounds stale
	// results from those sources and retries transient Git failures.
	if !now.Before(e.cachedAt.Add(r.cacheFallbackTTL)) {
		return cacheEntryExpired
	}
	if e.config.info == nil {
		return cacheEntryValid
	}
	info, err := r.stat(e.config.path)
	if err != nil {
		return cacheEntryStatFailed
	}
	if os.SameFile(e.config.info, info) &&
		e.config.info.Size() == info.Size() &&
		e.config.info.ModTime().Equal(info.ModTime()) {
		return cacheEntryValid
	}
	return cacheEntryChanged
}

func (r *Resolver) evictLeastRecentlyUsed() {
	var oldestPath string
	var oldestUse uint64
	for path, e := range r.cache {
		if oldestPath == "" || e.lastUsed < oldestUse {
			oldestPath, oldestUse = path, e.lastUsed
		}
	}
	if oldestPath != "" {
		delete(r.cache, oldestPath)
	}
}

func (r *Resolver) resolveGit(ctx context.Context, cwd string) (key, root, reason string, config configFingerprint) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.git(ctx, cwd, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", "", cwd + " is not a git repository", configFingerprint{}
	}
	config = r.gitConfigFingerprint(ctx, cwd)
	out, err := r.git(ctx, cwd, "remote", "get-url", "--all", "origin")
	if err != nil {
		return "", r.localRoot(ctx, cwd), cwd + " has no origin remote", config
	}
	urls := nonEmptyLines(out)
	switch {
	case len(urls) == 0:
		return "", r.localRoot(ctx, cwd), cwd + " has no origin remote", config
	case len(urls) > 1:
		return "", r.localRoot(ctx, cwd), cwd + " origin has multiple URLs", config
	}
	remote, err := gitremote.Canonicalize(urls[0], r.aliases)
	if err != nil {
		return "", r.localRoot(ctx, cwd), cwd + " origin URL is unrecognized: " + err.Error(), config
	}
	return remote.Key, "", "", config
}

func (r *Resolver) gitConfigFingerprint(ctx context.Context, cwd string) configFingerprint {
	path, err := r.git(ctx, cwd, "rev-parse", "--git-path", "config")
	path = strings.TrimSpace(path)
	if err != nil || path == "" {
		return configFingerprint{}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	info, err := r.stat(path)
	if err != nil || info.IsDir() {
		return configFingerprint{}
	}
	return configFingerprint{path: path, info: info}
}

// localRoot resolves the directory shared by every worktree of a no-remote repo:
// the main worktree, derived from git's common directory. Keying a standalone
// project on this (rather than the per-worktree cwd) collapses every live
// worktree and the main checkout of a local-only repo into one project, the same
// way a canonical remote collapses the worktrees of a repo that has one.
//
// It is best effort: a git too old to report the common dir, or a worktree whose
// link is already broken, yields "", and the server falls back to keying on the
// per-session cwd. The lookup runs only on the no-remote path, so a remote
// session never pays for it.
func (r *Resolver) localRoot(ctx context.Context, cwd string) string {
	out, err := r.git(ctx, cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return ""
	}
	// git reports the common dir relative to cwd from the main worktree and
	// absolute from a linked worktree; normalize both to one absolute, OS-native
	// path so every worktree of the repo produces the identical key.
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	dir = filepath.Clean(dir)
	// The common dir is "<main-worktree>/.git" for a normal repo, so its parent is
	// the main worktree: the friendlier key and display root. A bare repo has no
	// such parent, so its common dir stands as the key.
	if strings.EqualFold(filepath.Base(dir), ".git") {
		dir = filepath.Dir(dir)
	}
	return dir
}

// PeekHeader reads only as much of the file as it needs to extract cwd, the git
// branch, and a stable, unique source session id for the file. The id has to be
// unique per file: the server keys sessions on (user, agent, source_session_id),
// so two files that share an id fold into one row and clobber each other. Codex
// and pi files are already one-id-per-file, but Claude records the same
// sessionId in a main session file and in every subagent and workflow file under
// it, so those need an id derived from the file's location, not just its
// in-file sessionId.
//
// It also performs the positive session test: a file is a session only if at
// least one peeked line matches its agent's session signature. A parseable
// *.jsonl that never does (a tool-output log, an event feed under a custom
// extra_root) returns errNotSession so Resolve can skip it with a clear reason
// rather than upload it as a junk standalone/orphaned session.
func PeekHeader(f discover.File) (Header, error) {
	info, err := os.Stat(f.Path)
	if err != nil {
		return Header{}, err
	}
	if !info.Mode().IsRegular() {
		return Header{}, fmt.Errorf("session path %s is not a regular file", f.Path)
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return Header{}, err
	}
	defer file.Close()

	var h Header
	// Start from the filename-derived fallback; an in-file id overrides it below.
	h.sessionID = sourceIDFromName(f)

	sawSignature := false
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	const maxLines = 500 // cwd and the session signature appear early; cap the peek
	for i := 0; sc.Scan() && i < maxLines; i++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !gjson.Valid(line) {
			continue
		}
		e := gjson.Parse(line)
		applyHeaderLine(f.Agent, e, &h)
		if !sawSignature && sessionSignature(f.Agent, e) {
			sawSignature = true
		}
		if h.Cwd != "" && sawSignature {
			break // have both the field that gates resolution and proof it is a session
		}
	}
	if err := sc.Err(); err != nil {
		return Header{}, err
	}
	if !sawSignature {
		return Header{}, errNotSession
	}

	h.SourceID = sourceID(f, h.sessionID)
	return h, nil
}

// sessionSignature reports whether one parsed line looks like a genuine session
// record for the agent. It is the positive counterpart to discovery's suffix
// match: discovery accepts any *.jsonl (any rollout-*.jsonl for codex), and
// custom extra_roots point that walk at arbitrary trees, so without a content
// signature any parseable JSONL would resolve as a standalone or orphaned
// "session". Each agent's signature is a shape a real transcript always writes
// but unrelated JSONL does not: Claude's typed user/assistant entries carry a
// message object, Codex wraps every record in a payload, and pi opens with a
// session header (or writes typed message lines with a role).
func sessionSignature(agent string, e gjson.Result) bool {
	switch agent {
	case "claude":
		switch e.Get("type").String() {
		case "user", "assistant":
			return e.Get("message").Exists()
		}
		return false
	case "codex":
		switch e.Get("type").String() {
		case "session_meta", "response_item", "event_msg":
			return e.Get("payload").Exists()
		}
		return false
	case "pi":
		switch e.Get("type").String() {
		case "session":
			// The opening header. A real one carries the session metadata resolution
			// reads (its id and/or cwd); a bare {"type":"session"} does not.
			return e.Get("id").Exists() || e.Get("cwd").Exists()
		case "message":
			return e.Get("message.role").Exists()
		}
		return false
	}
	return false
}

// applyHeaderLine pulls header fields out of one parsed line for the agent. It
// records the raw in-file session id in h.sessionID; turning that into a unique
// SourceID is PeekHeader's job, since for Claude that depends on the file's path.
func applyHeaderLine(agent string, e gjson.Result, h *Header) {
	switch agent {
	case "claude":
		if v := e.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := e.Get("gitBranch").String(); v != "" {
			h.GitBranch = v
		}
		// Claude's in-file sessionId is deliberately ignored: it is not unique per
		// file (see sourceID). The path-derived fallback is the id.
	case "codex":
		p := e.Get("payload")
		if v := p.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := p.Get("git.branch").String(); v != "" {
			h.GitBranch = v
		}
		if v := p.Get("id").String(); v != "" {
			h.sessionID = v
		}
	case "pi":
		if v := e.Get("cwd").String(); v != "" {
			h.Cwd = v
		}
		if v := e.Get("id").String(); v != "" {
			h.sessionID = v
		}
	}
}

// sourceID turns a peeked header into a stable id that is unique per file. The
// server keys sessions on (user, agent, source_session_id), so two files sharing
// an id fold onto one row and clobber each other.
//
// Codex and pi write one id per file, so their in-file id stands. Claude is the
// exception twice over: every subagent and workflow file repeats its parent's
// sessionId, and a resumed or forked session writes a new file (named by a fresh
// id) that still records the original sessionId inside. Both make the in-file
// sessionId ambiguous, so Claude keys on the file's location, which it does keep
// unique: each session file has its own name and children nest under a per-session
// directory. Dropping the leading project directory (an encoded cwd) yields
// "<id>" for a main file and "<id>/subagents/..." for a child, which stays unique
// and keeps children grouped under their parent. An ordinary session, whose file
// is named by its sessionId, still resolves to exactly that id, so the scheme is
// unchanged except for the forked and child files it now keeps distinct.
func sourceID(f discover.File, sessionID string) string {
	if f.Agent != "claude" {
		return sessionID
	}
	rel := relPath(f.Root, f.Path)
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

// sourceIDFromName derives a stable source id from the file's location relative
// to its discovery root, used as the in-file fallback and for the Claude path
// suffix. Using the relative path rather than the bare basename keeps it unique:
// two workflow journal.jsonl files in different wf_* dirs would otherwise both
// collapse to "journal". The .jsonl suffix is stripped and separators are
// normalized to forward slashes so the id is identical across platforms.
func sourceIDFromName(f discover.File) string {
	return relPath(f.Root, f.Path)
}

// relPath returns path relative to root with forward-slash separators and the
// .jsonl extension stripped. If path is not under root (or root is empty), it
// falls back to the basename so the id is never an absolute path.
func relPath(root, path string) string {
	clean := strings.TrimSuffix(path, ".jsonl")
	if root != "" {
		if rel, err := filepath.Rel(root, clean); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Base(clean))
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
