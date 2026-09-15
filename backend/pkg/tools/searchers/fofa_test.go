package searchers

import (
	"context"
	"strings"
	"testing"

	"pentagi/pkg/config"
)

func newFofaForTest() *fofa {
	return &fofa{cfg: &config.Config{}}
}

func newFofaForTestWith(email, key string) *fofa {
	return &fofa{cfg: &config.Config{FofaEmail: email, FofaAPIKey: key}}
}

// TestFofaIsAvailable verifies the two-credential gate: FOFA requires
// BOTH email and api_key — a single one is not enough.
func TestFofaIsAvailable(t *testing.T) {
	tests := []struct {
		name  string
		email string
		key   string
		want  bool
	}{
		{"both set", "user@example.com", "abc123", true},
		{"email only", "user@example.com", "", false},
		{"key only", "", "abc123", false},
		{"both empty", "", "", false},
		{"whitespace only", "   ", "  ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFofaForTestWith(tc.email, tc.key)
			if got := s.IsAvailable(); got != tc.want {
				t.Errorf("IsAvailable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFofaBuildQueryStub documents the current stub behavior. Once the user
// implements the real classifier, update this test alongside buildQuery().
// Right now: pass-through with lowercasing + scheme strip.
func TestFofaBuildQueryStub(t *testing.T) {
	s := newFofaForTest()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already DSL passes through", `domain="example.com"`, `domain="example.com"`},
		{"scheme stripped", "https://Example.COM/", "example.com"},
		{"empty rejected", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.buildQuery(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("buildQuery(%q) expected error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildQuery(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("buildQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFofaFormatRendersMarkdownTable verifies the success-path rendering:
// header echo from FOFA + escaped pipes inside cells + truncation notice.
func TestFofaFormatRendersMarkdownTable(t *testing.T) {
	s := newFofaForTest()
	resp := &fofaResponse{
		Mode:    "normal",
		Total:   42,
		Fields:  strings.Split(defaultFofaFields, ","),
		Results: [][]string{
			{"api.example.com", "1.2.3.4", "443", "API Gateway", "example.com", "nginx/1.27", "Beijing", "Nginx", "Linux", "https"},
			{"pipe | in | cell", "5.6.7.8", "80", "Title with | pipe", "example.com", "Apache", "Shanghai", "Apache", "Linux", "http"},
		},
		QuotaFree: "1000",
	}
	out := s.format(`domain="example.com"`, resp, 25)

	// Header line: FOFA fields in order
	for _, want := range []string{"host", "ip", "port", "title", "domain"} {
		if !strings.Contains(out, want) {
			t.Errorf("format() missing column header %q\nfull output:\n%s", want, out)
		}
	}
	// Quota visibility so operator sees remaining budget
	if !strings.Contains(out, "1000") {
		t.Errorf("format() missing quota info\nfull output:\n%s", out)
	}
	// Pipe inside cell must be escaped, not break the table
	if !strings.Contains(out, `\|`) {
		t.Errorf("format() did not escape pipe in cell\nfull output:\n%s", out)
	}
	// Total shown so operator knows scope
	if !strings.Contains(out, "42") {
		t.Errorf("format() missing total count\nfull output:\n%s", out)
	}
}

// TestFofaFormatTruncationNotice: when FOFA reports more matches than we
// requested (or than the per-query cap), we surface a truncation warning.
func TestFofaFormatTruncationNotice(t *testing.T) {
	s := newFofaForTest()
	resp := &fofaResponse{
		Mode:   "normal",
		Total:  5000,
		Fields: strings.Split(defaultFofaFields, ","),
		Results: [][]string{
			{"a.example.com", "1.1.1.1", "443", "x", "example.com", "nginx", "Beijing", "Nginx", "Linux", "https"},
		},
	}
	out := s.format(`domain="example.com"`, resp, 1) // limit=1 → truncate
	if !strings.Contains(out, "4999 additional") && !strings.Contains(out, "truncated") {
		t.Errorf("format() missing truncation notice for Total=5000, limit=1\nfull output:\n%s", out)
	}
}

// TestFofaFormatEmptyResults: graceful empty-state output, no table header.
func TestFofaFormatEmptyResults(t *testing.T) {
	s := newFofaForTest()
	resp := &fofaResponse{
		Mode:    "normal",
		Total:   0,
		Fields:  strings.Split(defaultFofaFields, ","),
		Results: nil,
	}
	out := s.format(`domain="nothing.example"`, resp, 25)
	if !strings.Contains(out, "No assets matched") {
		t.Errorf("format() empty-state message missing\nfull output:\n%s", out)
	}
}

// TestFofaResponseIsSuccess: the success/failure gate from the FOFA JSON envelope.
func TestFofaResponseIsSuccess(t *testing.T) {
	tests := []struct {
		name string
		resp *fofaResponse
		want bool
	}{
		{"nil receiver", nil, false},
		{"error true", &fofaResponse{Error: true, Errmsg: "forbidden"}, false},
		{"error msg set", &fofaResponse{Errmsg: "auth failed"}, false},
		{"clean success", &fofaResponse{Error: false, Errmsg: ""}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resp.IsSuccess(); got != tc.want {
				t.Errorf("IsSuccess() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFofaEscapeCell: pipe neutralization for markdown safety.
func TestFofaEscapeCell(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"a|b|c", `a\|b\|c`},
		{"", ""},
	}
	for _, tc := range tests {
		if got := escapeCell(tc.in); got != tc.want {
			t.Errorf("escapeCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Compile-time guard that fofa satisfies the Searcher interface.
var _ Searcher = (*fofa)(nil)

// Avoid unused-import warning when the test grows; referencing context keeps
// the import honest without relying on the implementation.
var _ = context.Background
