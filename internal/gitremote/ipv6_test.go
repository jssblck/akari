package gitremote

import "testing"

func TestCanonicalizeIPv6Authorities(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"scheme", "ssh://git@[2001:db8::1]/Owner/Repo.git", "[2001:db8::1]/Owner/Repo"},
		{"custom port", "ssh://git@[2001:db8::1]:2222/owner/repo.git", "[2001:db8::1]:2222/owner/repo"},
		{"default port", "ssh://git@[2001:db8::1]:22/owner/repo.git", "[2001:db8::1]/owner/repo"},
		{"scp-like", "git@[2001:db8::1]:owner/repo.git", "[2001:db8::1]/owner/repo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote, err := Canonicalize(test.raw, nil)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if remote.Key != test.want {
				t.Fatalf("key = %q, want %q", remote.Key, test.want)
			}
		})
	}
}

func TestIPv6AddressAndPortCannotCollide(t *testing.T) {
	withPort, err := Canonicalize("ssh://git@[2001:db8::1]:2222/owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	addressOnly, err := Canonicalize("ssh://git@[2001:db8::1:2222]/owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if withPort.Key == addressOnly.Key {
		t.Fatalf("distinct IPv6 authorities collapsed to %q", withPort.Key)
	}
	if withPort.Host != "[2001:db8::1]:2222" || addressOnly.Host != "[2001:db8::1:2222]" {
		t.Fatalf("authorities = %q and %q", withPort.Host, addressOnly.Host)
	}
}

func TestUnbracketedIPv6SCPLikeRemoteIsRejected(t *testing.T) {
	if remote, err := Canonicalize("git@2001:db8::1:owner/repo.git", nil); err == nil {
		t.Fatalf("unbracketed IPv6 remote produced key %q", remote.Key)
	}
}

func TestSCPLikeRepositoryPathPreservesAtSign(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"hostname", "git@example.com:owner/repo@v2.git", "example.com/owner/repo@v2"},
		{"userless hostname", "example.com:owner/repo@v2.git", "example.com/owner/repo@v2"},
		{"at sign in username", "ada@example@host.example:owner/repo@v2.git", "host.example/owner/repo@v2"},
		{"IPv6", "git@[2001:db8::1]:owner/repo@v2.git", "[2001:db8::1]/owner/repo@v2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote, err := Canonicalize(test.raw, nil)
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if remote.Key != test.want {
				t.Fatalf("key = %q, want %q", remote.Key, test.want)
			}
		})
	}
}

func TestSCPLikeRepositoryPathPreservesColon(t *testing.T) {
	remote, err := Canonicalize("git@example.com:owner:team/repo.git", nil)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if remote.Key != "example.com/owner:team/repo" {
		t.Fatalf("key = %q, want example.com/owner:team/repo", remote.Key)
	}
}
