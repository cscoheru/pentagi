package searchers

import (
	"fmt"
	"strings"
	"testing"

	"pentagi/pkg/config"
)

func newCrtshForTest() *crtsh {
	return &crtsh{cfg: &config.Config{}}
}

func TestCrtshBuildQuery(t *testing.T) {
	s := newCrtshForTest()

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare domain gets wildcard", "example.com", "%.example.com", false},
		{"URL scheme stripped", "https://Example.COM/", "%.example.com", false},
		{"explicit star wildcard", "*.example.com", "%.example.com", false},
		{"explicit percent wildcard", "%.example.com", "%.example.com", false},
		{"uppercase lowered", "EXAMPLE.com", "%.example.com", false},
		{"no dot rejected", "localhost", "", true},
		{"spaces rejected", "two words", "", true},
		{"empty rejected", "   ", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.buildQuery(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buildQuery(%q) expected error, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildQuery(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("buildQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCrtshFormatDeduplicates(t *testing.T) {
	s := newCrtshForTest()

	base := crtshEntry{
		CommonName:     "example.com",
		NameValue:      "example.com\nwww.example.com",
		IssuerName:     "C=US, O=Let's Encrypt, CN=R3",
		ID:             1,
		EntryTimestamp: "2026-01-01T00:00:00Z",
		NotBefore:      "2025-12-01T00:00:00Z",
		NotAfter:       "2026-03-01T00:00:00Z",
		SerialNumber:   "AA:BB",
	}
	older := base            // same cert seen earlier in another log
	older.EntryTimestamp = "2025-06-01T00:00:00Z"
	older.ID = 99

	out := s.format("%.example.com", []crtshEntry{older, base})

	if strings.Count(out, "### example.com") != 1 {
		t.Fatalf("expected exactly one cert heading after dedupe, got:\n%s", out)
	}
	if !strings.Contains(out, "www.example.com") {
		t.Fatalf("expected SAN to be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "Let's Encrypt R3") {
		t.Fatalf("expected short issuer, got:\n%s", out)
	}
	if !strings.Contains(out, "https://crt.sh/?id=1") {
		t.Fatalf("expected newest entry (id=1) to win, got:\n%s", out)
	}
}

func TestCrtshFormatEmpty(t *testing.T) {
	s := newCrtshForTest()
	out := s.format("%.nope.invalid", nil)
	if !strings.Contains(out, "No certificates were found") {
		t.Fatalf("expected empty-result notice, got:\n%s", out)
	}
}

func TestCrtshFormatTruncatesBySize(t *testing.T) {
	s := newCrtshForTest()
	entries := make([]crtshEntry, 0, 500)
	for i := 0; i < 500; i++ {
		entries = append(entries, crtshEntry{
			CommonName:     fmt.Sprintf("%03d.%s.example.com", i, strings.Repeat("a", 20)),
			NameValue:      strings.Repeat("b", 300) + "\n" + strings.Repeat("c", 300) + "\n" + strings.Repeat("d", 300),
			IssuerName:     "C=US, O=Test Org, CN=Test CA",
			ID:             int64(i + 1),
			EntryTimestamp: "2026-01-01T00:00:00Z",
			SerialNumber:   "AA:00",
		})
	}
	out := s.format("%.example.com", entries)
	if len(out) > maxTotalResultSize+truncationMsgBuffer {
		t.Fatalf("output %d bytes exceeds cap %d", len(out), maxTotalResultSize)
	}
	if !strings.Contains(out, "truncated after") {
		t.Fatalf("expected truncation notice, got tail:\n%s", out[len(out)-200:])
	}
}

func TestCrtshEngineAndAvailability(t *testing.T) {
	s := newCrtshForTest()
	if string(s.Engine()) != "crtsh" {
		t.Fatalf("Engine() = %q", s.Engine())
	}
	if !s.IsAvailable() {
		t.Fatal("crtsh must always be available (keyless public API)")
	}
}

func TestShortIssuer(t *testing.T) {
	cases := map[string]string{
		"C=US, O=Let's Encrypt, CN=R3": "Let's Encrypt R3",
		"CN=Only CA":                   "Only CA",
		"":                             "",
	}
	for in, want := range cases {
		if got := shortIssuer(in); got != want {
			t.Fatalf("shortIssuer(%q) = %q, want %q", in, got, want)
		}
	}
}
