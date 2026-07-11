package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLogRotatesAtBoundaryAndContinuesWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	log, err := openLog(path, 12, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := log.Write([]byte("first record")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, path, "first record")
	if _, err := os.Stat(backupPath(path, 1)); !os.IsNotExist(err) {
		t.Fatalf("a file exactly at the boundary rotated early: %v", err)
	}

	if _, err := log.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, backupPath(path, 1), "first record")
	assertFileContents(t, path, "next\n")
}

func TestLogRetentionHasDeterministicBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	log, err := openLog(path, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	for _, record := range []string{"aaaa", "bbbb", "cccc", "dddd"} {
		if _, err := log.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	assertFileContents(t, backupPath(path, 2), "bbbb")
	assertFileContents(t, backupPath(path, 1), "cccc")
	assertFileContents(t, path, "dddd")
	if _, err := os.Stat(backupPath(path, 3)); !os.IsNotExist(err) {
		t.Fatalf("backup beyond retention exists: %v", err)
	}
	assertLogSetBound(t, path, 2, 12)
}

func TestLogRestartPreservesHistoryAndWindowsCompatibleHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	first, err := openLog(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("grace")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := openLog(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Write([]byte("ada")); err != nil {
		t.Fatalf("continued write after close-rename-reopen handoff: %v", err)
	}
	assertFileContents(t, backupPath(path, 1), "grace")
	assertFileContents(t, path, "ada")
}

func TestLogFilesRemainOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose ACL ownership through os.FileMode")
	}
	path := filepath.Join(t.TempDir(), "akari.log")
	if err := os.WriteFile(path, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath(path, 1), []byte("older"), 0o666); err != nil {
		t.Fatal(err)
	}
	log, err := openLog(path, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{path, backupPath(path, 1), backupPath(path, 2)} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s permissions = %04o, want 0600", name, got)
		}
	}
}

func TestLogCapsOversizedPreRotationFileOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := openLog(path, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	assertFileContents(t, path, "6789")
	assertLogSetBound(t, path, 1, 8)
}

func TestLogSerializesConcurrentRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.log")
	log, err := openLog(path, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	const writers = 32
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := fmt.Sprintf("record-%02d:%s\n", i, bytes.Repeat([]byte{'x'}, 128))
			if _, err := log.Write([]byte(record)); err != nil {
				t.Errorf("write record %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < writers; i++ {
		prefix := []byte(fmt.Sprintf("record-%02d:", i))
		if got := bytes.Count(data, prefix); got != 1 {
			t.Errorf("record %d appears %d times", i, got)
		}
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertLogSetBound(t *testing.T, path string, backups int, maxBytes int64) {
	t.Helper()
	var total int64
	for i := 0; i <= backups; i++ {
		name := path
		if i > 0 {
			name = backupPath(path, i)
		}
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > maxBytes {
		t.Fatalf("log set uses %d bytes, bound is %d", total, maxBytes)
	}
}
