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
		roots = append(roots, Root{Agent: r.Agent, Dir: r.Path})
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
// matching session-file symlinks are errors and are never followed. ex drops paths
// the user configured to exclude (pass the zero Excluder to keep everything).
func Discover(roots []Root, ex Excluder) ([]File, error) {
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
		info, err := os.Lstat(root.Dir)
		if err != nil {
			if root.Optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			problems = append(problems, fmt.Errorf("inspect %s root %s: %w", root.Agent, root.Dir, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			problems = append(problems, fmt.Errorf("inspect %s root %s: symlink roots are not allowed", root.Agent, root.Dir))
			continue
		}
		if !info.IsDir() {
			problems = append(problems, fmt.Errorf("inspect %s root %s: root is not a directory", root.Agent, root.Dir))
			continue
		}
		err = filepath.WalkDir(root.Dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// Prune an excluded directory so its whole subtree is skipped rather
				// than walked and filtered file by file.
				if path != root.Dir && ex.ExcludedDir(path) {
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
			out = append(out, File{Agent: root.Agent, Root: root.Dir, Path: path})
			return nil
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("walk %s root %s: %w", root.Agent, root.Dir, err))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(problems) > 0 {
		return out, &Error{problems: problems}
	}
	return out, nil
}
