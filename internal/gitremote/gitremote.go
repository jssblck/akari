// Package gitremote canonicalizes a git origin URL into akari's project key: a
// stable "authority/owner/.../repo" string that is identical across machines, clone
// URLs (ssh, https, scp-like), and worktrees. Keying projects by this canonical
// remote (not the local directory) is what collapses every checkout of a repo
// into one project.
package gitremote

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Remote is a canonicalized origin URL.
type Remote struct {
	Key   string // authority/owner/.../repo, the canonical project key
	Host  string // host authority, with IPv6 addresses bracketed
	Owner string // everything between host and repo (may contain slashes)
	Repo  string // final path segment
}

// caseInsensitiveHosts have case-insensitive paths, so their keys are lowercased
// to avoid splitting one repo into two projects over a capitalization difference.
// Other hosts keep their path case, which is the safe default.
var caseInsensitiveHosts = map[string]bool{
	"github.com":    true,
	"gitlab.com":    true,
	"bitbucket.org": true,
}

// defaultPorts are dropped from the key when they match the URL scheme, so
// ssh://host:22/... and host/... produce the same project.
var defaultPorts = map[string]string{
	"ssh":   "22",
	"https": "443",
	"http":  "80",
	"git":   "9418",
}

// Canonicalize turns an origin URL into a canonical project key. aliases maps an
// ssh Host alias to its real HostName (from LoadSSHAliases); pass nil to skip
// alias resolution. Alias resolution is best effort: an unresolved alias is left
// as-is, which at worst yields a duplicate project rather than a wrong merge.
func Canonicalize(raw string, aliases map[string]string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Remote{}, fmt.Errorf("empty remote URL")
	}

	host, port, path, err := split(raw)
	if err != nil {
		return Remote{}, err
	}

	// Only resolve short, dotless ssh aliases (for example "gh" -> github.com). A
	// host that already looks like a domain is never rewritten: doing so would turn
	// a canonical host into a transport endpoint (a common case is Host github.com
	// / HostName ssh.github.com / Port 443 for ssh-over-443) and split it from
	// https clones of the same repo. Leaving it as-is at worst yields a duplicate.
	if !strings.ContainsAny(host, ".:") {
		if real, ok := aliases[host]; ok && real != "" {
			host = real
		}
	}
	host = strings.ToLower(unbracketHost(host))
	bareHost := host
	authority := formatAuthority(host, port)

	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return Remote{}, fmt.Errorf("remote %q has no path", raw)
	}

	if caseInsensitiveHosts[bareHost] {
		path = strings.ToLower(path)
	}

	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return Remote{}, fmt.Errorf("remote %q has no owner/repo", raw)
	}
	for _, s := range segs {
		if s == "" {
			return Remote{}, fmt.Errorf("remote %q has an empty path segment", raw)
		}
	}
	repo := segs[len(segs)-1]
	owner := strings.Join(segs[:len(segs)-1], "/")

	return Remote{
		Key:   authority + "/" + path,
		Host:  authority,
		Owner: owner,
		Repo:  repo,
	}, nil
}

func formatAuthority(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func unbracketHost(host string) string {
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// split extracts host, optional non-default port, and path from a remote URL,
// dropping any scheme and userinfo (credentials). It accepts both scheme URLs
// (ssh://, https://, git://, ...) and the scp-like git@host:owner/repo form.
func split(raw string) (host, port, path string, err error) {
	if strings.Contains(raw, "://") {
		u, e := url.Parse(raw)
		if e != nil {
			return "", "", "", fmt.Errorf("parse remote %q: %w", raw, e)
		}
		host = u.Hostname() // strips userinfo and port
		if host == "" {
			return "", "", "", fmt.Errorf("remote %q has no host", raw)
		}
		port = u.Port()
		if dp, ok := defaultPorts[strings.ToLower(u.Scheme)]; ok && port == dp {
			port = ""
		}
		return host, port, u.Path, nil
	}

	// scp-like: [user@]host:path. IPv6 hosts must be bracketed so their address
	// colons cannot be mistaken for the host/path delimiter.
	hostPath := raw
	firstColon := strings.IndexByte(hostPath, ':')
	if firstColon >= 0 {
		if at := strings.LastIndexByte(hostPath[:firstColon], '@'); at >= 0 {
			hostPath = hostPath[at+1:]
		}
	}
	if strings.HasPrefix(hostPath, "[") {
		closeBracket := strings.IndexByte(hostPath, ']')
		if closeBracket < 0 || closeBracket+1 >= len(hostPath) || hostPath[closeBracket+1] != ':' {
			return "", "", "", fmt.Errorf("unrecognized bracketed remote URL %q", raw)
		}
		host = hostPath[1:closeBracket]
		path = hostPath[closeBracket+2:]
		if host == "" {
			return "", "", "", fmt.Errorf("remote %q has no host", raw)
		}
		return host, "", path, nil
	}
	if hasUnbracketedIPv6Prefix(hostPath) {
		return "", "", "", fmt.Errorf("IPv6 host in remote %q must be bracketed", raw)
	}
	colon := strings.IndexByte(hostPath, ':')
	if colon < 0 {
		return "", "", "", fmt.Errorf("unrecognized remote URL %q", raw)
	}
	host = hostPath[:colon]
	path = hostPath[colon+1:]
	if host == "" {
		return "", "", "", fmt.Errorf("remote %q has no host", raw)
	}
	return host, "", path, nil
}

// hasUnbracketedIPv6Prefix finds an IPv6 literal before a possible scp path
// delimiter. Checking address prefixes keeps colons in an ordinary repository
// path valid while rejecting the ambiguous unbracketed IPv6 form.
func hasUnbracketedIPv6Prefix(hostPath string) bool {
	segment := hostPath
	if slash := strings.IndexByte(segment, '/'); slash >= 0 {
		segment = segment[:slash]
	}
	for i := 0; i < len(segment); i++ {
		if segment[i] == ':' && isIPv6Literal(segment[:i]) {
			return true
		}
	}
	return isIPv6Literal(segment)
}

func isIPv6Literal(s string) bool {
	if zone := strings.LastIndexByte(s, '%'); zone >= 0 {
		s = s[:zone]
	}
	return strings.Contains(s, ":") && net.ParseIP(s) != nil
}

// LoadSSHAliases reads ~/.ssh/config and returns a map of Host alias to its
// resolved HostName. Missing or unreadable config yields an empty map; alias
// resolution is purely best effort.
func LoadSSHAliases() map[string]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return map[string]string{}
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return map[string]string{}
	}
	defer f.Close()
	return parseSSHAliases(f)
}

// parseSSHAliases extracts Host -> HostName mappings. Wildcard host patterns are
// ignored: only literal aliases can be substituted confidently.
func parseSSHAliases(r io.Reader) map[string]string {
	aliases := map[string]string{}
	var current []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitSSHLine(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			current = strings.Fields(val)
		case "hostname":
			for _, h := range current {
				if !strings.ContainsAny(h, "*?") {
					aliases[h] = val
				}
			}
		}
	}
	return aliases
}

// splitSSHLine splits an ssh_config "Key value" or "Key=value" directive.
func splitSSHLine(line string) (key, val string, ok bool) {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		key = strings.TrimSpace(line[:i])
		val = strings.TrimSpace(strings.TrimLeft(line[i:], " \t="))
		return key, val, key != "" && val != ""
	}
	return "", "", false
}
