package casenc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/parser"
	"github.com/klauspost/compress/zstd"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// withThreshold temporarily shrinks the compression threshold so both the raw and
// the compressed paths can be exercised on small test inputs.
func withThreshold(t *testing.T, n int) {
	t.Helper()
	orig := Threshold
	Threshold = n
	t.Cleanup(func() { Threshold = orig })
}

// decode decompresses a zstd frame for assertions.
func decode(t *testing.T, b []byte) []byte {
	t.Helper()
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(b, nil)
	if err != nil {
		t.Fatalf("decode zstd: %v", err)
	}
	return out
}

// TestEncodeBodyRawBelowThreshold confirms a body smaller than the threshold is
// stored verbatim and keyed by its raw hash.
func TestEncodeBodyRawBelowThreshold(t *testing.T) {
	withThreshold(t, 64)
	raw := []byte("small body")
	sha, stored, ct := Encoder{}.EncodeBody(raw)
	if ct != parser.ContentRaw {
		t.Fatalf("content type = %q, want %q", ct, parser.ContentRaw)
	}
	if !bytes.Equal(stored, raw) {
		t.Fatalf("stored bytes = %q, want the raw body verbatim", stored)
	}
	if sha != sha256Hex(raw) {
		t.Fatalf("key = %s, want sha256 of the raw body", sha)
	}
}

// TestEncodeBodyCompressedAtOrAboveThreshold confirms a body at or above the
// threshold is stored zstd and keyed by the hash of the compressed bytes, while the
// compressed bytes still decode to the original.
func TestEncodeBodyCompressedAtOrAboveThreshold(t *testing.T) {
	withThreshold(t, 64)
	raw := []byte(strings.Repeat("compress me ", 64)) // well over 64 bytes, highly compressible
	sha, stored, ct := Encoder{}.EncodeBody(raw)
	if ct != parser.ContentZstd {
		t.Fatalf("content type = %q, want %q", ct, parser.ContentZstd)
	}
	if bytes.Equal(stored, raw) {
		t.Fatal("stored bytes equal the raw body: not compressed")
	}
	if len(stored) >= len(raw) {
		t.Fatalf("compressed size %d not smaller than raw %d for repetitive input", len(stored), len(raw))
	}
	if sha != sha256Hex(stored) {
		t.Fatalf("key is not the hash of the stored (compressed) bytes")
	}
	if got := decode(t, stored); !bytes.Equal(got, raw) {
		t.Fatal("decompressed stored bytes do not match the raw body")
	}
}

// TestThresholdIsInclusiveLowerBound confirms the decision is exactly len(raw) >=
// Threshold: one byte under stays raw, exactly at the threshold compresses.
func TestThresholdIsInclusiveLowerBound(t *testing.T) {
	withThreshold(t, 100)
	under := bytes.Repeat([]byte("a"), 99)
	at := bytes.Repeat([]byte("a"), 100)

	if _, _, ct := (Encoder{}).EncodeBody(under); ct != parser.ContentRaw {
		t.Fatalf("99-byte body content type = %q, want raw", ct)
	}
	if _, _, ct := (Encoder{}).EncodeBody(at); ct != parser.ContentZstd {
		t.Fatalf("100-byte body content type = %q, want zstd", ct)
	}
}

// TestStreamMatchesInHand is the determinism contract: a body encoded by streaming
// (the big-line / digest path) yields the exact same key, content type, raw length,
// and stored bytes as the same body encoded in hand (the small-line path). If these
// diverged, the same body would land under two keys and dedup would break.
func TestStreamMatchesInHand(t *testing.T) {
	withThreshold(t, 64)
	cases := map[string][]byte{
		"raw-small":        []byte("tiny"),
		"compressed-large": []byte(strings.Repeat("payload-", 500)),
		"at-threshold":     bytes.Repeat([]byte("z"), 64),
	}
	enc := New()
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			wantSHA, wantStored, wantCT := enc.EncodeBody(raw)

			gotSHA, gotCT, rawLen, err := enc.HashStream(context.Background(), bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("hash stream: %v", err)
			}
			if gotSHA != wantSHA || gotCT != wantCT || rawLen != len(raw) {
				t.Fatalf("stream meta = (%s,%s,%d), in-hand = (%s,%s,%d)",
					gotSHA, gotCT, rawLen, wantSHA, wantCT, len(raw))
			}

			streamed, err := io.ReadAll(enc.StreamAs(context.Background(), bytes.NewReader(raw), gotCT))
			if err != nil {
				t.Fatalf("stream as: %v", err)
			}
			if !bytes.Equal(streamed, wantStored) {
				t.Fatalf("streamed stored bytes differ from in-hand stored bytes (len %d vs %d)",
					len(streamed), len(wantStored))
			}
			if sha256Hex(streamed) != wantSHA {
				t.Fatal("streamed stored bytes do not hash to the key")
			}
		})
	}
}

// TestEncodingIsDeterministic confirms encoding the same body twice produces
// byte-identical stored bytes, the property content addressing relies on.
func TestEncodingIsDeterministic(t *testing.T) {
	withThreshold(t, 16)
	raw := []byte(strings.Repeat("determinism ", 1000))
	_, a, _ := Encoder{}.EncodeBody(raw)
	_, b, _ := Encoder{}.EncodeBody(raw)
	if !bytes.Equal(a, b) {
		t.Fatal("two encodings of the same body differ: nondeterministic, would break dedup")
	}
}

// TestHashStreamCancel confirms a canceled context aborts a large streamed encode
// rather than running to the end.
func TestHashStreamCancel(t *testing.T) {
	withThreshold(t, 16)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))
	if _, _, _, err := New().HashStream(ctx, r); err == nil {
		t.Fatal("expected a context error from a canceled hash stream")
	}
}
