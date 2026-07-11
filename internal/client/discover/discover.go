// Package discover enumerates agent session files from each agent's known roots.
// It only locates files; reading their headers and resolving them to a project
// is the resolve package's job.
package discover

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jssblck/akari/internal/config"
	"github.com/tidwall/match"
)

// Excluder skips discovered paths that match any of a set of glob patterns,
// backing the config's `excludes` knob (config.Client.Excludes) so a user can
// keep a sensitive or noisy session directory from being discovered, watched, and
// uploaded. Patterns match against the full path with forward slashes, and `*`
// (or `**`) spans separators, so `**/tmp/**` excludes any path with a `tmp`
// segment and `*.private.jsonl` excludes by suffix anywhere. A zero Excluder (no
// patterns) excludes nothing, so callers that do not filter pass the zero value.
type Excluder struct {
	patterns []string
}

// NewExcluder compiles a set of exclude globs. Blank patterns are dropped and
// every pattern is normalized to forward slashes so a config written on one OS
// matches paths discovered on another.
func NewExcluder(globs []string) Excluder {
	patterns := make([]string, 0, len(globs))
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(g))
	}
	return Excluder{patterns: patterns}
}

// Excluded reports whether path matches any exclude pattern. The path is
// normalized to forward slashes before matching so the patterns are OS-agnostic.
func (e Excluder) Excluded(path string) bool {
	if len(e.patterns) == 0 {
		return false
	}
	p := filepath.ToSlash(path)
	for _, pat := range e.patterns {
		if match.Match(p, pat) {
			return true
		}
	}
	return false
}

// ExcludedDir reports whether a directory should be pruned. A directory is matched
// both bare and with a trailing slash so either pattern style prunes it: the bare
// form catches an exact pattern like `**/private`, and the trailing-slash form
// catches a subtree pattern like `**/tmp/**` (whose trailing `**` needs the slash
// to match the directory node itself, not just files under it).
func (e Excluder) ExcludedDir(path string) bool {
	return e.Excluded(path) || e.Excluded(path+"/")
}

// File is one discovered session file. Root is the discovery directory the file
// was found under: it is what lets resolution compute a stable, unique id from
// the file's location relative to the root, which is how Claude's subagent and
// workflow files (which all carry their parent's sessionId inside) avoid
// colliding on a single source id.
type File struct {
	Agent string // claude | codex | pi
	Root  string
	Path  string
}

// Root is a directory to scan for one agent's sessions.
type Root struct {
	Agent    string
	Dir      string
	Optional bool // a missing built-in default is expected when its agent is unused
	// FollowRootLink opts a root into resolving a symlink or (on Windows) a
	// directory junction at the root itself before walking, when the directory an
	// agent's sessions actually live under is only reachable through such a link
	// (for example a Windows user who relocated their session directory with
	// `mklink /J`). The default (false) rejects a linked root: see ResolveRoot.
	// The no-follow policy still applies to everything found inside the walk
	// regardless of this setting; it only ever affects the root path itself.
	FollowRootLink bool
}

// Roots builds the directories to scan for each agent. It honors each agent's own
// documented environment override (akari defines none of its own) via the env
// lookup, falls back to the standard location under home, and appends any extra
// roots from the config.
func Roots(cfg config.Client, env func(string) string, home string) []Root {
	var roots []Root

	if dir := env("CLAUDE_PROJECTS_DIR"); dir != "" {
		roots = append(roots, Root{Agent: "claude", Dir: dir})
	} else {
		roots = append(roots, Root{Agent: "claude", Dir: filepath.Join(home, ".claude", "projects"), Optional: true})
	}

	if dir := env("CODEX_SESSIONS_DIR"); dir != "" {
		roots = append(roots, Root{Agent: "codex", Dir: dir})
	} else {
		roots = append(roots,
			Root{Agent: "codex", Dir: filepath.Join(home, ".codex", "sessions"), Optional: true},
			Root{Agent: "codex", Dir: filepath.Join(home, ".codex", "archived_sessions"), Optional: true},
		)
	}

	if dir := env("PI_DIR"); dir != "" {
		roots = append(roots, Root{Agent: "pi", Dir: filepath.Join(dir, "agent", "sessions")})
	} else {
		roots = append(roots, Root{Agent: "pi", Dir: filepath.Join(home, ".pi", "agent", "sessions"), Optional: true})
	}

	for _, r := range cfg.ExtraRoots {
		roots = append(roots, Root{Agent: r.Agent, Dir: r.Path, FollowRootLink: r.FollowRootLink})
	}
	return roots
}

// Error reports every root or entry that could not be traversed safely. Discover
// still returns files from the complete portions of the scan so callers can make
// progress, but they must surface this error and finish unsuccessfully.
type Error struct {
	problems []error
}

func (e *Error) Error() string {
	return errors.Join(e.problems...).Error()
}

func (e *Error) Unwrap() []error {
	return e.problems
}

// ErrorCount returns the number of independent discovery failures represented
// by err. Non-discovery errors count as one so callers can use it at boundaries.
func ErrorCount(err error) int {
	if err == nil {
		return 0
	}
	var discoveryErr *Error
	if errors.As(err, &discoveryErr) {
		return len(discoveryErr.problems)
	}
	return 1
}

// Matches reports whether a filename is a session file for the given agent.
// Codex files are named rollout-*.jsonl; Claude and pi use any *.jsonl. This is
// only a suffix gate: every agent's files are further validated by a positive
// session-header signature at resolve time (see resolve.sessionSignature), which
// is what keeps unrelated *.jsonl under a custom extra_root from being ingested.
func Matches(agent, name string) bool {
	if !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	if agent == "codex" {
		return strings.HasPrefix(name, "rollout-")
	}
	return true
}

// Discover walks every root and returns the session files it finds, de-duplicated
// by path and sorted for stable output. A missing optional built-in root is quiet;
// missing configured roots and every other incomplete traversal are returned as
// errors alongside files found in complete portions of the scan. Symlink roots and
// matching session-file symlinks are errors and are never followed (see
// ResolveRoot for the one exception: a linked Optional built-in root is skipped
// with a notice rather than an error). notices carries those non-fatal skips so a
// caller can report them without counting them as failures. ex drops paths the
// user configured to exclude (pass the zero Excluder to keep everything).
func Discover(roots []Root, ex Excluder) (files []File, notices []string, err error) {
	seen := map[string]bool{}
	var out []File
	var problems []error
	for _, root := range roots {
		if root.Dir == "" {
			if !root.Optional {
				problems = append(problems, fmt.Errorf("discover %s root: path is empty", root.Agent))
			}
			continue
		}
		walkDir, notice, rerr := ResolveRoot(root)
		if rerr != nil {
			if root.Optional && errors.Is(rerr, fs.ErrNotExist) {
				continue
			}
			problems = append(problems, fmt.Errorf("inspect %s root %s: %w", root.Agent, root.Dir, rerr))
			continue
		}
		if notice != "" {
			notices = append(notices, notice)
			continue
		}
		werr := filepath.WalkDir(walkDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Prune an excluded directory so its whole subtree is skipped rather
				// than walked and filtered file by file.
				if path != walkDir && ex.ExcludedDir(path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !Matches(root.Agent, d.Name()) {
				return nil
			}
			if ex.Excluded(path) {
				return nil
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				problems = append(problems, fmt.Errorf("inspect session file %s: symlinks are not allowed", path))
				return nil
			}
			// Session readers use ordinary blocking file opens. Refuse pipes, devices,
			// and sockets before they can stall a whole sync.
			if !info.Mode().IsRegular() {
				return nil
			}
			if seen[path] {
				return nil
			}
			seen[path] = true
			out = append(out, File{Agent: root.Agent, Root: walkDir, Path: path})
			return nil
		})
		if werr != nil {
			problems = append(problems, fmt.Errorf("walk %s root %s: %w", root.Agent, root.Dir, werr))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(problems) > 0 {
		return out, notices, &Error{problems: problems}
	}
	return out, notices, nil
}

// ResolveRoot applies the closed root-link policy to one candidate root and
// reports what discovery should do with it. discover.Discover and
// watch.Watcher's addRecursive both call this so the two traversal paths cannot
// drift: a root that discovery would reject or skip is rejected or skipped
// identically when watch (re)adds its filesystem watches.
//
// A root that is a plain directory (the common case) resolves normally: dir is
// root.Dir. A root that is a symlink or, on Windows, a directory junction (see
// isWindowsReparsePoint) is a link standing in for the root, and is rejected by
// default: the closed policy that stops a link inside a walked root from being
// followed applies to the root itself for the same reason, escaping the
// configured location undetected. There is one exception, for the built-in
// per-agent default directories only (root.Optional): a linked Optional root is
// skipped with a non-fatal notice instead of an error, because those roots are
// opportunistic already (a missing one is silently skipped) and a user who
// junctioned their agent directory must opt in explicitly, but their sync must
// not start failing over it. Setting root.FollowRootLink resolves the link (any
// depth) to its target directory and walks that instead; the no-follow policy
// still applies to every path found inside the walk.
//
// The returned err is nil whenever dir or notice is set; it is non-nil only when
// the root cannot be used at all (missing, not a directory, or a rejected link).
func ResolveRoot(root Root) (dir string, notice string, err error) {
	class, err := classifyRoot(root.Dir)
	if err != nil {
		return "", "", err
	}
	if !class.linked {
		if !class.isDir {
			return "", "", errors.New("root is not a directory")
		}
		return root.Dir, "", nil
	}
	if !root.FollowRootLink {
		if root.Optional {
			return "", fmt.Sprintf("%s root %s is a %s; skipped (set follow_root_link on this root in the config to traverse it)", root.Agent, root.Dir, class.kind), nil
		}
		return "", "", fmt.Errorf("root is a %s (point the root at the real path, or set follow_root_link on this root to traverse it)", class.kind)
	}
	if !class.isDir {
		return "", "", fmt.Errorf("root is a %s whose target is not a directory", class.kind)
	}
	resolved, rerr := resolveRootLink(root.Dir)
	if rerr != nil {
		return "", "", fmt.Errorf("resolve %s target: %w", class.kind, rerr)
	}
	return resolved, "", nil
}

// rootClass is the result of inspecting one root candidate's Lstat.
type rootClass struct {
	linked bool   // the root itself is a symlink or a Windows directory junction
	kind   string // "symlink" or "directory junction", set when linked
	isDir  bool   // true when the root (or its link target, if linked) is a directory
}

// classifyRoot inspects dir and reports how it should be treated. It exists
// because Go's Lstat does not set ModeSymlink for a Windows directory junction
// (IO_REPARSE_TAG_MOUNT_POINT), only for a true NTFS symlink: a junction surfaces
// as ModeIrregular, so checking ModeSymlink alone misreports a junction root as
// "not a directory" rather than recognizing it as a link. isWindowsReparsePoint
// (platform-specific, see root_windows.go and root_other.go) closes that gap by
// reading the raw Windows file attributes; on every other OS it always reports
// false, since only NTFS has directory junctions.
func classifyRoot(dir string) (rootClass, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return rootClass{}, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return rootClass{linked: true, kind: "symlink", isDir: statIsDir(dir)}, nil
	case isWindowsReparsePoint(info):
		return rootClass{linked: true, kind: "directory junction", isDir: statIsDir(dir)}, nil
	default:
		return rootClass{isDir: info.IsDir()}, nil
	}
}

// statIsDir reports whether path resolves (following any link) to a directory.
func statIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// maxRootLinkDepth bounds resolveRootLink's walk so a link cycle fails cleanly
// instead of looping forever.
const maxRootLinkDepth = 32

// resolveRootLink follows a root's link chain to the real directory it names,
// used only once root.FollowRootLink has accepted a linked root. It does not use
// filepath.EvalSymlinks: that function only walks a path segment whose Mode
// reports ModeSymlink, which (as in classifyRoot) a Windows junction's
// IO_REPARSE_TAG_MOUNT_POINT does not set, so it silently returns a junction
// unresolved rather than following it. os.Readlink, which the standard library
// supports for both a true symlink and a junction, does not have that gap.
func resolveRootLink(dir string) (string, error) {
	cur := dir
	for range maxRootLinkDepth {
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 && !isWindowsReparsePoint(info) {
			return cur, nil
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
	}
	return "", fmt.Errorf("too many levels of links resolving root %s", dir)
}
