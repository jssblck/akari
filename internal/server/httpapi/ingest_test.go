package httpapi

import (
	"strings"
	"testing"
)

func TestLocalProjectKey(t *testing.T) {
	// Standalone and orphaned must share a key for the same machine+path so a
	// deleted folder transitions kind in place rather than forking a second row.
	a := localProjectKey("grace-laptop", "/home/grace/scratch")
	b := localProjectKey("grace-laptop", "/home/grace/scratch")
	if a != b {
		t.Fatalf("same machine+path produced different keys: %q vs %q", a, b)
	}
	// Different machine or path must differ.
	if localProjectKey("ada-box", "/home/grace/scratch") == a {
		t.Error("different machine produced same key")
	}
	if localProjectKey("grace-laptop", "/home/grace/other") == a {
		t.Error("different path produced same key")
	}
	// The "local:" prefix keeps synthetic keys out of the remote namespace: a
	// canonicalized git remote ("host/owner/repo") has no colon in its host, so it
	// can never equal a key of this shape.
	if !strings.HasPrefix(a, "local:") {
		t.Errorf("synthetic key %q lacks the local: prefix", a)
	}
}

func TestLastPathSegment(t *testing.T) {
	cases := map[string]string{
		"/home/grace/scratch":     "scratch",
		"/home/grace/scratch/":    "scratch",
		`C:\Users\grace\scratch`:  "scratch",
		`C:\Users\grace\scratch\`: "scratch",
		"scratch":                 "scratch",
		"":                        "",
		"/":                       "",
	}
	for in, want := range cases {
		if got := lastPathSegment(in); got != want {
			t.Errorf("lastPathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRemoteKey(t *testing.T) {
	cases := []struct {
		in                string
		host, owner, repo string
		ok                bool
	}{
		{"github.com/jssblck/akari", "github.com", "jssblck", "akari", true},
		{"gitlab.com/group/subgroup/proj", "gitlab.com", "group/subgroup", "proj", true},
		{"github.com/onlyowner", "", "", "", false},
		{"", "", "", "", false},
		{"github.com//repo", "", "", "", false},
		{"/owner/repo", "", "", "", false},
	}
	for _, c := range cases {
		host, owner, repo, ok := parseRemoteKey(c.in)
		if ok != c.ok || host != c.host || owner != c.owner || repo != c.repo {
			t.Errorf("parseRemoteKey(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, host, owner, repo, ok, c.host, c.owner, c.repo, c.ok)
		}
	}
}
