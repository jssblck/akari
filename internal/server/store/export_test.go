package store

// This file is the test-only seam between the store package and its black-box
// tests in package store_test. Those tests provision a real database through the
// storetest package, which imports store; a white-box (package store) test could
// not import storetest without a cycle, so the integration tests live in
// store_test and reach the few internals they need through the names below. The
// file is compiled only into the test binary, so none of this ships in the server.

// SetRebuildRegionBytes overrides the rebuild's raw-region batch size and returns
// a function that restores the previous value, so a test can force a rebuild to
// feed the reducer several bounded regions with a single deferred call:
// `defer SetRebuildRegionBytes(n)()`.
func SetRebuildRegionBytes(n int64) (restore func()) {
	old := rebuildRegionBytes
	rebuildRegionBytes = n
	return func() { rebuildRegionBytes = old }
}

// SetSettledSignalBatch overrides the settle-pass batch size and returns a function that
// restores the previous value, so a test can force the multi-batch keyset drain without
// seeding hundreds of sessions: `defer SetSettledSignalBatch(n)()`.
func SetSettledSignalBatch(n int) (restore func()) {
	old := settledSignalBatch
	settledSignalBatch = n
	return func() { settledSignalBatch = old }
}

// WriteBlobTx exposes the transactional CAS writer so a test can insert and hold
// a blob row inside an open transaction (used to exercise the sweep's writer lock
// and to seed an attachment body).
var WriteBlobTx = writeBlobTx

// SanitizeText exposes the message-text sanitizer for its unit test.
var SanitizeText = sanitizeText
