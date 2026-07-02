package store

import "testing"

// TestSessionRelPath pins the worktree-invariant key derivation: an absolute path under the cwd
// loses the prefix, a path outside the cwd stays absolute-only (not ok), an already-relative path
// passes through normalized, and the Windows drive-letter case quirk is tolerated on the drive but
// nowhere else. The ("", true) case is explicitly excluded so an ok result always carries a key.
func TestSessionRelPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		cwd      string
		filePath string
		wantRel  string
		wantOK   bool
	}{
		{
			name:     "posix absolute under cwd",
			cwd:      "/home/grace/akari",
			filePath: "/home/grace/akari/internal/x.go",
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "posix absolute outside cwd stays absolute only",
			cwd:      "/home/grace/akari",
			filePath: "/etc/hosts",
			wantOK:   false,
		},
		{
			name:     "sibling sharing a name prefix is not under cwd",
			cwd:      "/home/grace/akari",
			filePath: "/home/grace/akari-two/internal/x.go",
			wantOK:   false,
		},
		{
			name:     "windows absolute under cwd, matching drive case",
			cwd:      `C:\Users\me\projects\akari`,
			filePath: `C:\Users\me\projects\akari\internal\x.go`,
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "windows absolute under cwd, differing drive-letter case",
			cwd:      `C:\Users\me\projects\akari`,
			filePath: `c:\Users\me\projects\akari\internal\x.go`,
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "windows worktree cwd differs from project cwd, same rel path",
			cwd:      `C:\Users\me\projects\worktrees\akari\foo`,
			filePath: `C:\Users\me\projects\worktrees\akari\foo\internal\x.go`,
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "non-drive path segment case is significant",
			cwd:      "/home/grace/Akari",
			filePath: "/home/grace/akari/internal/x.go",
			wantOK:   false,
		},
		{
			name:     "backslash input normalizes to forward slashes",
			cwd:      `/home/grace/akari`,
			filePath: `\home\grace\akari\internal\x.go`,
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "already-relative path passes through normalized",
			cwd:      "/home/grace/akari",
			filePath: `internal\x.go`,
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "already-relative path with no cwd still ok",
			cwd:      "",
			filePath: "internal/x.go",
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "dot-slash prefix trimmed",
			cwd:      "/home/grace/akari",
			filePath: "./internal/x.go",
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "trailing slash on cwd is tolerated",
			cwd:      "/home/grace/akari/",
			filePath: "/home/grace/akari/internal/x.go",
			wantRel:  "internal/x.go",
			wantOK:   true,
		},
		{
			name:     "empty cwd with an absolute path is not ok",
			cwd:      "",
			filePath: "/home/grace/akari/internal/x.go",
			wantOK:   false,
		},
		{
			name:     "empty path is not ok",
			cwd:      "/home/grace/akari",
			filePath: "",
			wantOK:   false,
		},
		{
			name:     "path equal to cwd yields no key",
			cwd:      "/home/grace/akari",
			filePath: "/home/grace/akari",
			wantOK:   false,
		},
		{
			name:     "path equal to cwd with trailing slash yields no key",
			cwd:      "/home/grace/akari",
			filePath: "/home/grace/akari/",
			wantOK:   false,
		},
		{
			name:     "dot-slash only yields no key",
			cwd:      "/home/grace/akari",
			filePath: "./",
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rel, ok := sessionRelPath(tc.cwd, tc.filePath)
			if ok != tc.wantOK {
				t.Fatalf("sessionRelPath(%q, %q) ok = %v, want %v", tc.cwd, tc.filePath, ok, tc.wantOK)
			}
			if !ok {
				if rel != "" {
					t.Fatalf("sessionRelPath(%q, %q) returned %q with ok=false, want empty", tc.cwd, tc.filePath, rel)
				}
				return
			}
			if rel != tc.wantRel {
				t.Fatalf("sessionRelPath(%q, %q) = %q, want %q", tc.cwd, tc.filePath, rel, tc.wantRel)
			}
		})
	}
}
