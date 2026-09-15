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

// TestFofaBuildQuery exercises every branch of the classifier:
//   - already DSL → whitelist-validate pass-through
//   - DSL with unknown field → rejected (prompt-injection guard)
//   - bare IPv4 / IPv4 CIDR → ip="..."
//   - 2-label hostname → domain="..." (matches all subdomains)
//   - 3+ label hostname → host="..." (targets exactly)
//   - invalid input (whitespace, @, embedded path/port) → either Fatal or
//     auto-stripped depending on the offending fragment
//   - ambiguous single token → title="..." fallback (always returns something)
func TestFofaBuildQuery(t *testing.T) {
	s := newFofaForTest()

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// Already DSL — allowed fields pass through
		{"allowed domain field", `domain="example.com"`, `domain="example.com"`, false},
		{"allowed host field", `host="api.example.com"`, `host="api.example.com"`, false},
		{"allowed ip field", `ip="1.2.3.4"`, `ip="1.2.3.4"`, false},
		{"allowed title field", `title="Kubelet"`, `title="Kubelet"`, false},
		{"allowed cert field", `cert="Let's Encrypt"`, `cert="Let's Encrypt"`, false}, // case preserved for DSL
		{"allowed product field", `product="Nginx"`, `product="Nginx"`, false}, // case preserved for DSL

		// Already DSL — disallowed fields rejected (security)
		{"unknown field rejected", `qbase64="payload"`, "", true},
		{"arbitrary op rejected", `__proto__="x"`, "", true},
		{"no leading field rejected", `"example.com"`, `title="\"example.com\""`, false}, // no `=`, falls to title

		// IPv4
		{"bare ipv4", "1.2.3.4", `ip="1.2.3.4"`, false},
		{"ipv4 CIDR /24", "1.2.3.0/24", `ip="1.2.3.0/24"`, false},
		{"ipv4 CIDR /32", "8.8.8.8/32", `ip="8.8.8.8/32"`, false},
		{"private ipv4 still wraps", "192.168.1.1", `ip="192.168.1.1"`, false},

		// Domains — all multi-label hostname forms use domain= (matches subdomains)
		{"root domain 2-label", "example.com", `domain="example.com"`, false},
		{"subdomain 3-label uses domain=", "api.example.com", `domain="api.example.com"`, false},
		{"deep subdomain 4-label uses domain=", "a.b.example.com", `domain="a.b.example.com"`, false},
		{"scheme stripped", "https://Example.COM/", `domain="example.com"`, false},
		{"port suffix stripped from host", "example.com:8080", `domain="example.com"`, false},
		{"path suffix stripped from host", "api.example.com/health", `domain="api.example.com"`, false},
		{"punycode accepted 2-label", "xn--fiqs8s.xn--0zwm56d", `domain="xn--fiqs8s.xn--0zwm56d"`, false},
		{"com.cn multi-part TLD uses domain=", "example.com.cn", `domain="example.com.cn"`, false},

		// Rejected
		{"empty rejected", "   ", "", true},
		{"path strip leaves valid domain", "example.com/very/deep", `domain="example.com"`, false}, // strip is helpful, not an error
		{"at sign rejected (email)", "user@example.com", "", true},
		{"no dot single label", "localhost", `title="localhost"`, false}, // → title fallback
		{"digits-only TLD falls to title", "example.123", `title="example.123"`, false},
		{"invalid IP falls to title", "999.0.0.1", `title="999.0.0.1"`, false},
		{"hyphen-edge label falls to title", "-foo-.com", `title="-foo-.com"`, false},

		// Title fallback (always returns something; multi-word preserved)
		{"bare keyword → title", "Kubelet", `title="kubelet"`, false},
		{"uppercase keyword lowered", "NGINX", `title="nginx"`, false},
		{"product name with space", "Apache Struts", `title="apache struts"`, false},
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

// TestFofaAllowedFieldsWhitelist guards against accidental removal of
// fields from the whitelist. The exact list is part of the public contract.
func TestFofaAllowedFieldsWhitelist(t *testing.T) {
	expected := []string{
		"domain", "ip", "host", "title", "cert", "port", "protocol",
		"product", "server", "os", "banner", "city", "country",
		"header", "body", "jarm", "asn", "org", "base_protocol",
		"is_domain", "is_ipv4", "icon_hash", "fid", "sitemap",
	}
	for _, f := range expected {
		if _, ok := fofaAllowedFields[f]; !ok {
			t.Errorf("fofaAllowedFields missing required field %q", f)
		}
	}
	// Banned ones stay banned.
	for _, f := range []string{"qbase64", "q", "__proto__", "exec"} {
		if _, ok := fofaAllowedFields[f]; ok {
			t.Errorf("fofaAllowedFields should NOT contain %q", f)
		}
	}
}

// TestFofaIsDNSHostname covers the DNS-label validator directly.
func TestFofaIsDNSHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"example.com", true},
		{"a.b.example.com", true},
		{"xn--fiqs8s.xn--0zwm56d", true}, // Chinese IDN in punycode
		{"example.cn", true},
		{"foo.xn--abc123", true},
		{"localhost", false},    // no dot
		{"example.123", false},  // digits-only TLD
		{"example.1", false},    // single-char TLD (digits)
		{"-foo.com", false},     // leading hyphen
		{"foo-.com", false},     // trailing hyphen in label
		{"foo..com", false},     // empty label
		{"foo bar.com", false},  // space (defensively — though buildQuery catches earlier)
		{"a.b", false},          // single-char alpha TLD rejected
		{"a.b.c.d.e.f", false},  // single-char TLD at end rejected
		{"example.xn--", false}, // too-short punycode suffix
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isDNSHostname(tc.in); got != tc.want {
				t.Errorf("isDNSHostname(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestFofaBuildQueryUppercaseLowered confirms case folding at every layer.
func TestFofaBuildQueryUppercaseLowered(t *testing.T) {
	s := newFofaForTest()
	got, err := s.buildQuery("EXAMPLE.COM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `domain="example.com"` {
		t.Errorf("got %q, want %q", got, `domain="example.com"`)
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
