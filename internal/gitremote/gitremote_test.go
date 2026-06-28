package gitremote

import (
	"strings"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // canonical key, or "" if an error is expected
	}{
		{"scp-like", "git@github.com:owner/repo.git", "github.com/owner/repo"},
		{"ssh-scheme", "ssh://git@github.com/owner/repo.git", "github.com/owner/repo"},
		{"https", "https://github.com/owner/repo.git", "github.com/owner/repo"},
		{"https-no-suffix", "https://github.com/owner/repo", "github.com/owner/repo"},
		{"https-credentials", "https://user:token@github.com/owner/repo.git", "github.com/owner/repo"},
		{"git-scheme", "git://github.com/owner/repo", "github.com/owner/repo"},
		{"ssh-default-port", "ssh://git@github.com:22/owner/repo.git", "github.com/owner/repo"},
		{"https-default-port", "https://github.com:443/owner/repo.git", "github.com/owner/repo"},
		{"ssh-custom-port", "ssh://git@example.com:2222/owner/repo.git", "example.com:2222/owner/repo"},
		{"uppercase-host", "git@GitHub.com:Owner/Repo.git", "github.com/owner/repo"},
		{"case-insensitive-path-lowered", "https://github.com/Owner/Repo", "github.com/owner/repo"},
		{"case-sensitive-host-preserved", "https://git.example.com/Owner/Repo", "git.example.com/Owner/Repo"},
		{"nested-subgroups", "https://gitlab.com/group/sub/repo.git", "gitlab.com/group/sub/repo"},
		{"bitbucket-lowered", "git@bitbucket.org:Team/Proj.git", "bitbucket.org/team/proj"},
		{"trailing-slash", "https://github.com/owner/repo/", "github.com/owner/repo"},
		{"empty", "", ""},
		{"no-path", "https://github.com", ""},
		{"no-owner", "https://github.com/repo", ""},
		{"garbage", "not a url", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Canonicalize(c.raw, nil)
			if c.want == "" {
				if err == nil {
					t.Fatalf("expected error, got key %q", got.Key)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Key != c.want {
				t.Errorf("key = %q, want %q", got.Key, c.want)
			}
		})
	}
}

func TestCanonicalizeFields(t *testing.T) {
	got, err := Canonicalize("https://gitlab.com/group/sub/repo.git", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "gitlab.com" || got.Owner != "group/sub" || got.Repo != "repo" {
		t.Errorf("fields = %+v", got)
	}
}

func TestCanonicalizeSSHAlias(t *testing.T) {
	aliases := map[string]string{"gh": "github.com"}
	got, err := Canonicalize("git@gh:owner/repo.git", aliases)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "github.com/owner/repo" {
		t.Errorf("alias not resolved: key = %q", got.Key)
	}

	// An unknown alias is left as-is rather than guessed.
	got, err = Canonicalize("git@unknown:owner/repo.git", aliases)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "unknown/owner/repo" {
		t.Errorf("unknown alias should pass through: key = %q", got.Key)
	}
}

// TestCanonicalizeDoesNotRewriteRealHosts guards against the ssh-over-443 case:
// a Host github.com / HostName ssh.github.com alias must not rewrite the
// canonical host, which would split ssh and https clones into two projects.
func TestCanonicalizeDoesNotRewriteRealHosts(t *testing.T) {
	aliases := map[string]string{"github.com": "ssh.github.com"}
	got, err := Canonicalize("git@github.com:owner/repo.git", aliases)
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "github.com/owner/repo" {
		t.Errorf("canonical host was rewritten: key = %q, want github.com/owner/repo", got.Key)
	}
}

func TestParseSSHAliases(t *testing.T) {
	cfg := `
# a comment
Host gh github
    HostName github.com
    User git

Host work
  HostName git.internal.example.com

Host *.proxy
  HostName should-be-ignored
`
	aliases := parseSSHAliases(strings.NewReader(cfg))
	if aliases["gh"] != "github.com" {
		t.Errorf("gh -> %q, want github.com", aliases["gh"])
	}
	if aliases["github"] != "github.com" {
		t.Errorf("github -> %q, want github.com (multiple aliases on one Host line)", aliases["github"])
	}
	if aliases["work"] != "git.internal.example.com" {
		t.Errorf("work -> %q", aliases["work"])
	}
	if _, ok := aliases["*.proxy"]; ok {
		t.Error("wildcard host patterns must be ignored")
	}
}
