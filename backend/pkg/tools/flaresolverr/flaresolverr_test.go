package flaresolverr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCookieJarScopesToExactHost(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.json")
	raw := map[string][]map[string]string{
		// A leading-dot domain in the source must not survive: Chrome's
		// addCookie rejects it for the exact page host.
		"tieba.baidu.com": {
			{"name": "BDUSS", "value": "secret-value", "domain": ".baidu.com"},
			{"name": "TIEBA_SID", "value": "sid"},
		},
	}
	data, _ := json.Marshal(raw)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	jar, err := LoadCookieJar(path)
	if err != nil {
		t.Fatalf("LoadCookieJar: %v", err)
	}

	cookies, err := jar.CookiesForURL("https://tieba.baidu.com/f?kw=test")
	if err != nil {
		t.Fatalf("CookiesForURL: %v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2", len(cookies))
	}
	for _, c := range cookies {
		if c.Domain != "tieba.baidu.com" {
			t.Errorf("cookie %q has domain %q, want exact host", c.Name, c.Domain)
		}
	}

	if cookies[0].Value != "secret-value" {
		t.Errorf("cookie value = %q", cookies[0].Value)
	}
}

func TestCookieJarMissingFileIsEmpty(t *testing.T) {
	t.Parallel()

	jar, err := LoadCookieJar(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing jar should not error: %v", err)
	}
	cookies, err := jar.CookiesForURL("https://example.test")
	if err != nil || cookies != nil {
		t.Fatalf("missing jar should yield no cookies, got %v %v", cookies, err)
	}
}

func TestCookieJarUnknownHost(t *testing.T) {
	t.Parallel()

	jar := NewCookieJar()
	cookies, err := jar.CookiesForURL("https://other.example/path")
	if err != nil || cookies != nil {
		t.Fatalf("unknown host should yield no cookies, got %v %v", cookies, err)
	}
}

func TestHTMLToMarkdownDropsScriptsAndKeepsContent(t *testing.T) {
	t.Parallel()

	html := `<!doctype html><html><head><style>.x{}</style></head>
<body>
<script>var secret = 1;</script>
<nav>home about</nav>
<main>
<h1>Forum title</h1>
<p>First <strong>paragraph</strong> with a <a href="https://example.test/p/123">thread</a>.</p>
<ul><li>one</li><li>two</li></ul>
</main></body></html>`

	md := HTMLToMarkdown(html)
	if !contains(md, "Forum title") {
		t.Errorf("markdown missing heading: %q", md)
	}
	if !contains(md, "thread") || !contains(md, "https://example.test/p/123") {
		t.Errorf("markdown missing link: %q", md)
	}
	if !contains(md, "- one") || !contains(md, "- two") {
		t.Errorf("markdown missing list: %q", md)
	}
	if contains(md, "var secret") {
		t.Errorf("markdown kept the script: %q", md)
	}
	if contains(md, "home about") {
		t.Errorf("markdown kept the nav: %q", md)
	}
}

func TestClientUnavailableWithoutURL(t *testing.T) {
	t.Parallel()

	c := NewClient("")
	if c.Available() {
		t.Fatal("empty URL must not be available")
	}
	if _, err := c.Fetch(t.Context(), "https://example.test"); err == nil {
		t.Fatal("fetch on unconfigured client must error")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
