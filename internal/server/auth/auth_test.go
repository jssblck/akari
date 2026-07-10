package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashVerifyPassword(t *testing.T) {
	const pw = "correct horse battery staple"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := VerifyPassword(pw, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword (wrong): %v", err)
	}
	if ok {
		t.Fatal("wrong password verified")
	}
}

func TestHashPasswordSaltsDiffer(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("identical passwords produced identical hashes; salt missing")
	}
}

func TestVerifyPasswordRejectsGarbage(t *testing.T) {
	if _, err := VerifyPassword("x", "not-a-phc-string"); err != ErrInvalidHash {
		t.Fatalf("want ErrInvalidHash, got %v", err)
	}
}

func TestVerifyPasswordRejectsUnsafeParameters(t *testing.T) {
	valid, err := HashPassword("Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, "$")
	tests := map[string]string{
		"zero time":        strings.Replace(valid, "t=3", "t=0", 1),
		"zero threads":     strings.Replace(valid, "p=2", "p=0", 1),
		"excessive memory": strings.Replace(valid, "m=65536", "m=4294967295", 1),
		"wrong version":    strings.Replace(valid, "v=19", "v=18", 1),
		"trailing params":  strings.Replace(valid, "p=2", "p=2junk", 1),
		"oversized salt":   strings.Join([]string{"", parts[1], parts[2], parts[3], strings.Repeat("A", 1<<20), parts[5]}, "$"),
		"oversized key":    strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], strings.Repeat("A", 1<<20)}, "$"),
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("VerifyPassword panicked: %v", recovered)
				}
			}()
			ok, err := VerifyPassword("Ada Lovelace", encoded)
			if ok || !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("VerifyPassword() = (%v, %v), want (false, %v)", ok, err, ErrInvalidHash)
			}
		})
	}
}

func TestNewTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatal("duplicate token generated")
		}
		seen[tok] = true
	}
}

func TestHashTokenStable(t *testing.T) {
	first, second := HashToken("abc"), HashToken("abc")
	if first != second {
		t.Fatal("HashToken not deterministic")
	}
	if HashToken("abc") == HashToken("abd") {
		t.Fatal("HashToken collision on distinct inputs")
	}
}
