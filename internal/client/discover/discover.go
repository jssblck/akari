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
)

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
// Codex files are named rollout-*.jsonl; Claude and pi use any *.jsonl (pi files
// are further validated by their session header at resolve time).
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
// user does not run simply contribute nothing.
func Discover(roots []Root) ([]File, error) {
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
			if d.IsDir() || !Matches(root.Agent, d.Name()) {
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
