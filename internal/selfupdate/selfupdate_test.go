package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		bin, version, goos, goarch, want string
	}{
		{"akari", "0.1.0", "linux", "amd64", "akari_0.1.0_linux_amd64.tar.gz"},
		{"akari", "0.1.0", "darwin", "arm64", "akari_0.1.0_darwin_arm64.tar.gz"},
		{"akari", "0.1.0", "windows", "amd64", "akari_0.1.0_windows_amd64.zip"},
		{"akari-server", "1.2.3", "linux", "arm64", "akari-server_1.2.3_linux_arm64.tar.gz"},
		// A leading v on the version is stripped to match the release filenames.
		{"akari", "v2.0.0", "linux", "amd64", "akari_2.0.0_linux_amd64.tar.gz"},
	}
	for _, c := range cases {
		if got := AssetName(c.bin, c.version, c.goos, c.goarch); got != c.want {
			t.Errorf("AssetName(%q,%q,%q,%q) = %q, want %q", c.bin, c.version, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestUpToDate(t *testing.T) {
	cases := []struct {
		current, latest     string
		wantUp, wantCompare bool
	}{
		{"v0.1.0", "v0.1.0", true, true},
		{"v0.1.0", "v0.2.0", false, true},
		{"v0.2.0", "v0.1.0", true, true}, // ahead of latest: do not downgrade
		{"v1.0.0", "v1.0.1", false, true},
		{"dev", "v0.1.0", false, false}, // development build: not comparable
		{"abc123-dirty", "v0.1.0", false, false},
	}
	for _, c := range cases {
		up, cmp := UpToDate(c.current, c.latest)
		if up != c.wantUp || cmp != c.wantCompare {
			t.Errorf("UpToDate(%q,%q) = (%v,%v), want (%v,%v)", c.current, c.latest, up, cmp, c.wantUp, c.wantCompare)
		}
	}
}

func TestLatestTag(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/grace/hopper/releases/latest" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v1.2.3","name":"v1.2.3"}`)
	}))
	defer ts.Close()

	c := &Client{Repo: "grace/hopper", HTTP: ts.Client(), APIBaseURL: ts.URL}
	tag, err := c.LatestTag(context.Background())
	if err != nil {
		t.Fatalf("LatestTag: %v", err)
	}
	if tag != "v1.2.3" {
		t.Errorf("LatestTag = %q, want v1.2.3", tag)
	}
}

func TestFetchTarGz(t *testing.T) {
	payload := []byte("\x7fELF fake linux binary")
	archive := makeTarGz(t, "akari", payload)
	dest := filepath.Join(t.TempDir(), "akari")

	c := fetchTestClient(t, "akari_1.2.3_linux_amd64.tar.gz", archive)
	if err := c.Fetch(context.Background(), "akari", "v1.2.3", "linux", "amd64", dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, payload) {
		t.Errorf("extracted binary = %q, want %q", got, payload)
	}
}

func TestFetchZip(t *testing.T) {
	payload := []byte("MZ fake windows binary")
	archive := makeZip(t, "akari.exe", payload)
	dest := filepath.Join(t.TempDir(), "akari.exe")

	c := fetchTestClient(t, "akari_1.2.3_windows_amd64.zip", archive)
	if err := c.Fetch(context.Background(), "akari", "v1.2.3", "windows", "amd64", dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, payload) {
		t.Errorf("extracted binary = %q, want %q", got, payload)
	}
}

func TestWriteBinaryRejectsOversizeInsteadOfInstallingTruncatedFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "akari")
	err := writeBinaryLimit(dest, bytes.NewReader([]byte("0123456789")), 8)
	if err == nil {
		t.Fatal("writeBinaryLimit accepted an oversized binary")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("oversized destination remains after failure: %v", statErr)
	}
}

func TestGetRejectsOversizedMetadataInsteadOfAcceptingValidPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.3"}`+strings.Repeat(" ", 32)+"trailing")
	}))
	t.Cleanup(server.Close)

	client := New()
	_, err := client.getLimit(context.Background(), server.URL, "application/json", 32)
	if err == nil || !strings.Contains(err.Error(), "exceeds 32-byte limit") {
		t.Fatalf("getLimit oversized metadata error = %v, want explicit limit error", err)
	}
}

func TestDownloadToFileRejectsOversizeAndRemovesPartialArchive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
	}))
	t.Cleanup(server.Close)

	dest := filepath.Join(t.TempDir(), "release.tar.gz")
	client := New()
	err := client.downloadToFileLimit(context.Background(), server.URL, dest, 8)
	if err == nil || !strings.Contains(err.Error(), "exceeds 8-byte limit") {
		t.Fatalf("downloadToFileLimit oversized archive error = %v, want explicit limit error", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("oversized partial archive remains after failure: %v", statErr)
	}
}

func TestExtractRejectsNonRegularBinaryEntries(t *testing.T) {
	t.Run("tar directory", func(t *testing.T) {
		archivePath := filepath.Join(t.TempDir(), "akari.tar.gz")
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "akari", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extractTarGz(archivePath, "akari", filepath.Join(t.TempDir(), "dest")); err == nil {
			t.Fatal("extractTarGz accepted a directory as the binary")
		}
	})

	t.Run("tar hard link", func(t *testing.T) {
		archivePath := filepath.Join(t.TempDir(), "akari.tar.gz")
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "akari", Typeflag: tar.TypeLink, Linkname: "real-akari", Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extractTarGz(archivePath, "akari", filepath.Join(t.TempDir(), "dest")); err == nil {
			t.Fatal("extractTarGz accepted a hard link as the binary")
		}
	})

	t.Run("zip directory", func(t *testing.T) {
		archivePath := filepath.Join(t.TempDir(), "akari.zip")
		var archive bytes.Buffer
		zw := zip.NewWriter(&archive)
		if _, err := zw.Create("akari.exe/"); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extractZip(archivePath, "akari.exe", filepath.Join(t.TempDir(), "dest.exe")); err == nil {
			t.Fatal("extractZip accepted a directory as the binary")
		}
	})
}

func TestExtractZipReadsThroughChecksum(t *testing.T) {
	payload := []byte("MZ Grace Hopper")
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	header := &zip.FileHeader{Name: "akari.exe", Method: zip.Store}
	w, err := zw.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := archive.Bytes()
	at := bytes.Index(raw, payload)
	if at < 0 {
		t.Fatal("stored zip payload not found")
	}
	raw[at] ^= 0xff
	archivePath := filepath.Join(t.TempDir(), "akari.zip")
	if err := os.WriteFile(archivePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = extractZip(archivePath, "akari.exe", filepath.Join(t.TempDir(), "dest.exe"))
	if err == nil {
		t.Fatal("extractZip accepted an entry with a bad CRC")
	}
	if err != zip.ErrChecksum && !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("extractZip CRC error = %v, want %v", err, zip.ErrChecksum)
	}
}

func TestFetchChecksumMismatch(t *testing.T) {
	archive := makeTarGz(t, "akari", []byte("real contents"))
	asset := "akari_1.2.3_linux_amd64.tar.gz"
	// Serve a SHA256SUMS that lists the wrong digest so verification must fail.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			w.Write(archive)
		case "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", "0000000000000000000000000000000000000000000000000000000000000000", asset)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c := &Client{Repo: "grace/hopper", HTTP: ts.Client(), DownloadBaseURL: ts.URL}
	err := c.Fetch(context.Background(), "akari", "v1.2.3", "linux", "amd64", filepath.Join(t.TempDir(), "akari"))
	if err == nil {
		t.Fatal("Fetch succeeded on a checksum mismatch, want error")
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "akari")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".akari-update")
	if err := os.WriteFile(staged, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Replace(target, staged); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new binary" {
		t.Errorf("target after Replace = %q, want %q", got, "new binary")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged file still present after Replace; should have been moved")
	}
	CleanupOld(target)
}

// fetchTestClient returns a Client whose download base serves the given archive
// at its asset name and a matching SHA256SUMS.
func fetchTestClient(t *testing.T, asset string, archive []byte) *Client {
	t.Helper()
	sum := sha256.Sum256(archive)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			w.Write(archive)
		case "/SHA256SUMS":
			fmt.Fprint(w, sums)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return &Client{Repo: "grace/hopper", HTTP: ts.Client(), DownloadBaseURL: ts.URL}
}

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	// A second entry mirrors the real archive, which also ships README.md.
	readme := []byte("akari")
	if err := tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(readme))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(readme); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
