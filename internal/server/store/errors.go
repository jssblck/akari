package store

import "errors"

// ErrNotFound is returned by lookups that match no row.
var ErrNotFound = errors.New("not found")

// ErrInvalidInvite is returned when registration presents an invite token that
// is unknown, already redeemed, or expired.
var ErrInvalidInvite = errors.New("invalid or already used invite token")
