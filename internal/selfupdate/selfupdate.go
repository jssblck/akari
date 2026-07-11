// Package selfupdate resolves and downloads akari client releases from GitHub
// so the client can update itself in place.
//
// It is the Go counterpart of the install scripts in scripts/: it resolves the
// latest published release, downloads the archive that matches a target OS and
// architecture, verifies it against the release SHA256SUMS, and extracts the
// binary. The client uses it for a fully native update with no shell or curl.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultRepo is the GitHub owner/repo releases are pulled from. AKARI_REPO
// overrides it, matching the install scripts.
const DefaultRepo = "jssblck/akari"

const (
	// maxMetadata caps the API and SHA256SUMS reads; both are small.
	maxMetadata = 4 << 20 // 4 MiB
	// maxArchive caps both a downloaded release archive and its extracted binary.
	maxArchive = 512 << 20 // 512 MiB
)

// Client resolves and downloads release assets. The zero value is not usable;
// construct one with New.
type Client struct {
	// Repo is the GitHub owner/repo to download from.
	Repo string
	// HTTP is the client used for all requests.
	HTTP *http.Client
	// APIBaseURL is the base for the GitHub API. Overridable for testing.
	APIBaseURL string
	// DownloadBaseURL, when non-empty, is the base URL holding the archive and
	// SHA256SUMS, overriding the per-tag GitHub release URL. It mirrors the
	// AKARI_BASE_URL the install scripts honor, and lets tests point at a local
	// server.
	DownloadBaseURL string
}

// New builds a Client with the default GitHub endpoints, honoring the AKARI_REPO
// and AKARI_BASE_URL environment overrides the install scripts also respect.
func New() *Client {
	repo := os.Getenv("AKARI_REPO")
	if repo == "" {
		repo = DefaultRepo
	}
	return &Client{
		Repo:            repo,
		HTTP:            &http.Client{},
		APIBaseURL:      "https://api.github.com",
		DownloadBaseURL: os.Getenv("AKARI_BASE_URL"),
	}
}

// LatestTag returns the tag of the latest published release (the same release
// the install scripts resolve through the GitHub API).
func (c *Client) LatestTag(ctx context.Context) (string, error) {
	body, err := c.get(ctx, c.APIBaseURL+"/repos/"+c.Repo+"/releases/latest", "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("parse latest release: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no published release found for %s", c.Repo)
	}
	return rel.TagName, nil
}

// AssetName is the release archive filename for a binary and target, matching
// the names the release workflow produces: the version without a leading v, a
// .zip for Windows, and a .tar.gz everywhere else.
func AssetName(binName, version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", binName, strings.TrimPrefix(version, "v"), goos, goarch, ext)
}

// Fetch downloads the release archive for binName at tag for the given target,
// verifies it against the release SHA256SUMS, and extracts the binary to dest
// (created with mode 0755). It does not touch the running binary; the caller
// installs the result with Replace.
func (c *Client) Fetch(ctx context.Context, binName, tag, goos, goarch, dest string) error {
	version := strings.TrimPrefix(tag, "v")
	asset := AssetName(binName, version, goos, goarch)
	base := c.downloadBase(tag)

	sums, err := c.get(ctx, base+"/SHA256SUMS", "")
	if err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	want, err := checksumFor(sums, asset)
	if err != nil {
		return err
	}

	archive, err := os.CreateTemp("", "akari-archive-*")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	archive.Close()
	defer os.Remove(archivePath)

	if err := c.downloadToFile(ctx, base+"/"+asset, archivePath); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	got, err := sha256File(archivePath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum mismatch for %s (want %s, got %s)", asset, want, got)
	}

	entry := binName
	if goos == "windows" {
		entry += ".exe"
	}
	if goos == "windows" {
		return extractZip(archivePath, entry, dest)
	}
	return extractTarGz(archivePath, entry, dest)
}

// downloadBase is the base URL holding the archive and SHA256SUMS for a tag.
func (c *Client) downloadBase(tag string) string {
	if c.DownloadBaseURL != "" {
		return strings.TrimSuffix(c.DownloadBaseURL, "/")
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s", c.Repo, tag)
}

func (c *Client) get(ctx context.Context, url, accept string) ([]byte, error) {
	return c.getLimit(ctx, url, accept, maxMetadata)
}

func (c *Client) getLimit(ctx context.Context, url, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "akari-selfupdate")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte limit", url, limit)
	}
	return body, nil
}

func (c *Client) downloadToFile(ctx context.Context, url, dest string) error {
	return c.downloadToFileLimit(ctx, url, dest, maxArchive)
}

func (c *Client) downloadToFileLimit(ctx context.Context, url, dest string, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "akari-selfupdate")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(dest)
		return copyErr
	}
	if written > limit {
		os.Remove(dest)
		return fmt.Errorf("GET %s: response exceeds %d-byte limit", url, limit)
	}
	if closeErr != nil {
		os.Remove(dest)
		return closeErr
	}
	return nil
}

// checksumFor returns the hex digest listed for asset in a SHA256SUMS body. The
// file has one "<hex>  <name>" line per asset.
func checksumFor(sums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s in SHA256SUMS", asset)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz writes the tar.gz entry whose base name is entry to dest.
func extractTarGz(archivePath, entry, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive did not contain %s", entry)
		}
		if err != nil {
			return err
		}
		if path.Base(hdr.Name) == entry {
			if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != 0 {
				return fmt.Errorf("archive entry %s is not a regular file", entry)
			}
			if hdr.Size > maxArchive {
				return fmt.Errorf("archive entry %s exceeds %d-byte limit", entry, maxArchive)
			}
			return writeBinary(dest, tr)
		}
	}
}

// extractZip writes the zip entry whose base name is entry to dest.
func extractZip(archivePath, entry, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if path.Base(zf.Name) == entry {
			if !zf.FileInfo().Mode().IsRegular() {
				return fmt.Errorf("archive entry %s is not a regular file", entry)
			}
			if zf.UncompressedSize64 > maxArchive {
				return fmt.Errorf("archive entry %s exceeds %d-byte limit", entry, maxArchive)
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeBinary(dest, rc)
		}
	}
	return fmt.Errorf("archive did not contain %s", entry)
}

// writeBinary writes r to dest as an executable file, replacing any existing
// dest. dest should sit in the same directory as the binary it will replace so
// the follow-up rename in Replace stays on one filesystem.
func writeBinary(dest string, r io.Reader) error {
	return writeBinaryLimit(dest, r, maxArchive)
}

func writeBinaryLimit(dest string, r io.Reader, limit int64) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(dest), err)
	}
	written, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		f.Close()
		os.Remove(dest)
		return err
	}
	if written > limit {
		f.Close()
		os.Remove(dest)
		return fmt.Errorf("archive entry exceeds %d-byte limit", limit)
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}
