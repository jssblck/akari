package frontend

import (
	"bytes"
	"testing"
)

func TestEmbeddedBuildContainsHashedApplicationEntry(t *testing.T) {
	index, err := Index()
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	if !bytes.Contains(index, []byte(`/app-assets/assets/index-`)) {
		t.Fatalf("embedded index does not reference a production asset: %s", index)
	}
	if bytes.Contains(index, []byte("/src/main.tsx")) {
		t.Fatal("embedded index references the Vite development entry")
	}
}

func TestDocumentAddsEscapedPublicMetadata(t *testing.T) {
	document, err := Document(Metadata{
		Title:       `Grace & Ada <session>`,
		Description: `A shared "agent" session`,
		URL:         `https://akari.example/s/abc?x=1&y=2`,
		Image:       `https://akari.example/s/abc/og.png`,
	})
	if err != nil {
		t.Fatalf("build document: %v", err)
	}
	for _, want := range []string{
		`<title>Grace &amp; Ada &lt;session&gt;</title>`,
		`property="og:description" content="A shared &#34;agent&#34; session"`,
		`rel="canonical" href="https://akari.example/s/abc?x=1&amp;y=2"`,
		`name="twitter:card" content="summary_large_image"`,
	} {
		if !bytes.Contains(document, []byte(want)) {
			t.Errorf("document does not contain %q: %s", want, document)
		}
	}
	if bytes.Contains(document, []byte(`<session>`)) {
		t.Fatal("document contains an unescaped metadata value")
	}
}
