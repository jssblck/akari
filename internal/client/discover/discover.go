// Package discover enumerates agent session files from each agent's known roots.
// It only locates files; reading their headers and resolving them to a project
// is the resolve package's job.
package discover

import (
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
	Agent string
	Dir   string
}

// Roots builds the directories to scan for each agent. It honors each agent's own
// documented environment override (akari defines none of its own) via the env
// lookup, falls back to the standard location under home, and appends any extra
// roots from the config.
func Roots(cfg config.Client, env func(string) string, home string) []Root {
	var roots []Root

	if dir := env("CLAUDE_PROJECTS_DIR"); dir != "" {
		roots = append(roots, Root{"claude", dir})
	} else {
		roots = append(roots, Root{"claude", filepath.Join(home, ".claude", "projects")})
	}

	if dir := env("CODEX_SESSIONS_DIR"); dir != "" {
		roots = append(roots, Root{"codex", dir})
	} else {
		roots = append(roots,
			Root{"codex", filepath.Join(home, ".codex", "sessions")},
			Root{"codex", filepath.Join(home, ".codex", "archived_sessions")},
		)
	}

	if dir := env("PI_DIR"); dir != "" {
		roots = append(roots, Root{"pi", filepath.Join(dir, "agent", "sessions")})
	} else {
		roots = append(roots, Root{"pi", filepath.Join(home, ".pi", "agent", "sessions")})
	}

	for _, r := range cfg.ExtraRoots {
		roots = append(roots, Root{r.Agent, r.Path})
	}
	return roots
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
// by path and sorted for stable output. A missing root is not an error: agents a
// user does not run simply contribute nothing. ex drops paths the user configured
// to exclude (pass the zero Excluder to keep everything): an excluded directory is
// pruned from the walk, and an excluded file is skipped, so an ignored location is
// never discovered, watched, or uploaded.
func Discover(roots []Root, ex Excluder) ([]File, error) {
	seen := map[string]bool{}
	var out []File
	for _, root := range roots {
		if root.Dir == "" {
			continue
		}
		if info, err := os.Stat(root.Dir); err != nil || !info.IsDir() {
			continue
		}
		err := filepath.WalkDir(root.Dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries rather than aborting the walk
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
			if seen[path] {
				return nil
			}
			seen[path] = true
			out = append(out, File{Agent: root.Agent, Root: root.Dir, Path: path})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
