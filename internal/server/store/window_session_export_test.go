package store

// SetWindowSessionRowsReadHookForTest pauses WindowSessionPage after its visible
// rows are read and before its remainder is aggregated.
func (s *Store) SetWindowSessionRowsReadHookForTest(hook func()) {
	s.windowSessionRowsReadHook = hook
}
