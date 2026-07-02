package auth

import "testing"

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
