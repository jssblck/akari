package store

// SweepBlobBatchSize exposes the sweep's page size so the cancellation test can
// seed exactly one full batch plus one blocked row without hardcoding 256.
const SweepBlobBatchSize = sweepBlobBatchSize

// SetSweepBatchCommittedHookForTest installs the per-batch commit observer on
// the sweep loop. The hook runs on the sweeping goroutine after each batch's
// commit is acknowledged and its count is folded into the sweep's running
// total, so anything the hook triggers (such as a context cancel) can no
// longer cost that batch its place in the reported count.
func (s *Store) SetSweepBatchCommittedHookForTest(hook func(batchRemoved int)) {
	s.sweepBatchCommittedHook = hook
}
