package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// LogMaxBytes keeps the active daemon log useful without letting a
	// workstation process consume unbounded disk space.
	LogMaxBytes int64 = 5 << 20
	// LogBackups is the number of complete rotated daemon logs retained beside
	// the active file.
	LogBackups = 3
)

// renameFile performs the renames rotation depends on. It is a variable so
// tests can force a rotation failure (a locked file, a permission error)
// without needing an OS condition that is awkward to reproduce reliably,
// especially on Windows where the failure this guards against is common.
var renameFile = os.Rename

// RotatingLog is the sole writer for a daemon log. Rotation closes the active
// handle before renaming it, which is required while the daemon is running on
// Windows. The mutex keeps each Write and its rotation handoff indivisible.
type RotatingLog struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	backups  int

	// droppedRecords and lastRotationErr track a rotation failure streak so it
	// can be surfaced in the log itself instead of vanishing behind the
	// log.Logger.Printf caller, which discards Write's returned error.
	droppedRecords  int64
	lastRotationErr error
}

// OpenLog opens the built-in daemon's bounded, owner-only log set.
func OpenLog(path string) (*RotatingLog, error) {
	return openLog(path, LogMaxBytes, LogBackups)
}

func openLog(path string, maxBytes int64, backups int) (*RotatingLog, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("daemon log maximum size must be positive")
	}
	if backups < 0 {
		return nil, fmt.Errorf("daemon log backup count must not be negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create daemon log directory: %w", err)
	}
	if err := secureHistory(path, maxBytes, backups); err != nil {
		return nil, err
	}
	if err := secureLogPath(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secure daemon log %s: %w", path, err)
	}
	if err := capExistingLog(path, maxBytes); err != nil {
		return nil, err
	}

	l := &RotatingLog{path: path, maxBytes: maxBytes, backups: backups}
	if err := l.openActive(); err != nil {
		return nil, err
	}
	return l, nil
}

func secureHistory(path string, maxBytes int64, backups int) error {
	for i := 1; i <= backups; i++ {
		name := backupPath(path, i)
		if err := secureLogPath(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("secure daemon log %s: %w", name, err)
		}
		if err := capExistingLog(name, maxBytes); err != nil {
			return err
		}
	}
	return nil
}

// capExistingLog handles an active file left by a version without rotation.
// Keeping its newest bytes gives the upgraded daemon a deterministic bound
// immediately, before it writes the first new record.
func capExistingLog(path string, maxBytes int64) (returnErr error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open existing daemon log %s: %w", path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close existing daemon log %s: %w", path, err))
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat existing daemon log %s: %w", path, err)
	}
	if info.Size() <= maxBytes {
		return nil
	}
	if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
		return fmt.Errorf("seek existing daemon log %s: %w", path, err)
	}
	tail := make([]byte, maxBytes)
	if _, err := io.ReadFull(f, tail); err != nil {
		return fmt.Errorf("read existing daemon log %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind existing daemon log %s: %w", path, err)
	}
	if _, err := f.Write(tail); err != nil {
		return fmt.Errorf("rewrite existing daemon log %s: %w", path, err)
	}
	if err := f.Truncate(maxBytes); err != nil {
		return fmt.Errorf("truncate existing daemon log %s: %w", path, err)
	}
	return nil
}

func (l *RotatingLog) openActive() error {
	f, err := openSecureLogFile(l.path)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", l.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat daemon log %s: %w", l.path, err)
	}
	l.file = f
	l.size = info.Size()
	return nil
}

// Write appends all bytes in order, rotating before a record would cross the
// size boundary. An unusually large single write is split across bounded files
// without dropping bytes that still fit within the retained history.
//
// When rotation fails, the record that triggered it is dropped rather than
// written through: writing through would defeat the disk bound rotation
// exists to enforce. The drop is not silent, though. It is counted, and as
// soon as a rotation next succeeds (in this call or a later one), or a plain
// write finds a drop still unreported, a synthesized notice line is written
// ahead of the caller's record so an operator reading the log can see how
// much was lost and why.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return 0, os.ErrClosed
	}
	written := 0
	noticeAttempted := false
	for len(p) > 0 {
		if l.size > 0 && (l.size == l.maxBytes || int64(len(p)) > l.maxBytes-l.size) {
			if err := l.rotate(); err != nil {
				l.droppedRecords++
				l.lastRotationErr = err
				return written, err
			}
		}
		if l.droppedRecords > 0 && !noticeAttempted {
			// Bypass the normal boundary check: routing this through the same
			// rotate-then-write path could fail the same way rotation just
			// failed and recurse into reporting a failure to report a failure.
			// Letting the notice push the file slightly past maxBytes is the
			// safer tradeoff. Looping back re-evaluates the boundary check
			// against the post-notice size before any real record is written.
			// noticeAttempted caps this to once per call: if the notice write
			// itself keeps failing, the caller's record still gets a chance
			// to go out, and the streak is retried on the next call.
			noticeAttempted = true
			l.writeDropNotice()
			continue
		}
		space := l.maxBytes - l.size
		chunk := int64(len(p))
		if chunk > space {
			chunk = space
		}
		n, err := l.file.Write(p[:int(chunk)])
		l.size += int64(n)
		written += n
		p = p[n:]
		if err != nil {
			return written, fmt.Errorf("write daemon log %s: %w", l.path, err)
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

// writeDropNotice appends one record naming how many records prior rotation
// failures dropped and the most recent error, then clears the streak. If the
// write itself fails, the streak is left intact so the next successful
// rotation or write tries again instead of losing the count.
func (l *RotatingLog) writeDropNotice() {
	notice := fmt.Sprintf("akari: log rotation failed %d times; %d records dropped; last error: %v\n",
		l.droppedRecords, l.droppedRecords, l.lastRotationErr)
	n, err := l.file.Write([]byte(notice))
	l.size += int64(n)
	if err != nil {
		return
	}
	l.droppedRecords = 0
	l.lastRotationErr = nil
}

func (l *RotatingLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close daemon log for rotation: %w", err)
	}
	l.file = nil

	if l.backups == 0 {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return l.reopenAfterRotationError(fmt.Errorf("remove daemon log during rotation: %w", err))
		}
		return l.openActive()
	}

	oldest := backupPath(l.path, l.backups)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return l.reopenAfterRotationError(fmt.Errorf("remove oldest daemon log %s: %w", oldest, err))
	}
	for i := l.backups - 1; i >= 1; i-- {
		from := backupPath(l.path, i)
		to := backupPath(l.path, i+1)
		if err := renameFile(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return l.reopenAfterRotationError(fmt.Errorf("rotate daemon log %s to %s: %w", from, to, err))
		}
	}
	first := backupPath(l.path, 1)
	if err := renameFile(l.path, first); err != nil && !errors.Is(err, os.ErrNotExist) {
		return l.reopenAfterRotationError(fmt.Errorf("rotate daemon log %s to %s: %w", l.path, first, err))
	}
	if err := secureLogPath(first); err != nil && !errors.Is(err, os.ErrNotExist) {
		return l.reopenAfterRotationError(fmt.Errorf("secure rotated daemon log %s: %w", first, err))
	}
	return l.openActive()
}

func (l *RotatingLog) reopenAfterRotationError(rotationErr error) error {
	if err := l.openActive(); err != nil {
		return errors.Join(rotationErr, fmt.Errorf("reopen daemon log after failed rotation: %w", err))
	}
	return rotationErr
}

func backupPath(path string, generation int) string {
	return fmt.Sprintf("%s.%d", path, generation)
}

// Close flushes and closes the active log. It is safe to call more than once.
func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("close daemon log %s: %w", l.path, err)
	}
	return nil
}
