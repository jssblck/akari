package httpapi

import "testing"

func TestParseRemoteKey(t *testing.T) {
	cases := []struct {
		in                 string
		host, owner, repo  string
		ok                 bool
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
