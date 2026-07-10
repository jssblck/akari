// Package auth holds akari-server's credential primitives: argon2id password
// hashing, opaque token and id generation, and token hashing. It is pure crypto
// with no storage or HTTP dependencies.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Tuned for an interactive login on server hardware; the
// chosen values are encoded into every hash so they can change without breaking
// existing passwords.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrInvalidHash is returned when a stored password hash cannot be parsed.
var ErrInvalidHash = errors.New("invalid password hash format")

// HashPassword returns a PHC-encoded argon2id hash with a fresh random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the PHC-encoded hash, using
// the parameters and salt embedded in the hash. The comparison is
// constant-time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// "" / argon2id / v=.. / m=..,t=..,p=.. / salt / hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version || parts[2] != fmt.Sprintf("v=%d", version) {
		return false, ErrInvalidHash
	}
	var mem uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}
	if parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", mem, time, threads) ||
		time == 0 || time > argonTime ||
		threads == 0 || threads > argonThreads ||
		mem < 8*uint32(threads) || mem > argonMemory {
		return false, ErrInvalidHash
	}
	// Stored hashes are an untrusted database boundary. Bound the encoded fields
	// before decoding them so a corrupt row cannot force a large allocation, then
	// require the exact sizes HashPassword emits before invoking Argon2.
	if len(parts[4]) != base64.RawStdEncoding.EncodedLen(argonSaltLen) ||
		len(parts[5]) != base64.RawStdEncoding.EncodedLen(argonKeyLen) {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != argonKeyLen {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NewToken returns a new opaque secret (URL-safe, 256 bits of entropy) suitable
// for an API token, invite token, or session cookie id.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewPublicID returns an unguessable, URL-safe id for a published session (144
// bits of entropy). Unlike a token it is stored in the clear: it is a capability
// URL, so possession of the link is what grants logged-out read access.
func NewPublicID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex sha256 of a presented secret. Only hashes are
// stored, so a database read never exposes a usable token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
