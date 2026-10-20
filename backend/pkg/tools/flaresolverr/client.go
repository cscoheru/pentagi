// Package flaresolverr is a minimal client for a FlareSolverr instance.
//
// FlareSolverr drives a real Chrome, which is required for sites that render
// their content client-side after a JavaScript challenge. The browser tool
// falls back to it when the primary scraper meets a known challenge page.
//
// Authentication is supplied from a server-side cookie jar, never from tool
// call arguments: the model only asks for a URL, and the client attaches the
// cookies configured for that host.
package flaresolverr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultTimeout bounds a single request to FlareSolverr. A real challenge
// solve routinely takes 25-60s, so this is not a short timeout.
const (
	DefaultTimeout      = 75 * time.Second
	defaultMaxTimeoutMS = 60000
)

// Client talks to a FlareSolverr server.
type Client struct {
	baseURL    string
	httpClient *http.Client
	maxTimeout int
	jar        *CookieJar
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// WithMaxTimeout sets the per-solve maxTimeout sent to FlareSolverr
// (milliseconds). The outer HTTP timeout must be larger.
func WithMaxTimeout(milliseconds int) Option {
	return func(c *Client) {
		if milliseconds > 0 {
			c.maxTimeout = milliseconds
		}
	}
}

// WithCookieJar attaches a cookie jar used to authenticate requests.
func WithCookieJar(jar *CookieJar) Option {
	return func(c *Client) {
		c.jar = jar
	}
}

// NewClient builds a client for the given FlareSolverr base URL
// (e.g. "http://flaresolverr:8191"). An empty URL produces a client whose
// requests fail with a clear error, so callers can construct it unconditionally.
func NewClient(baseURL string, options ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: DefaultTimeout},
		maxTimeout: defaultMaxTimeoutMS,
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// Available reports whether a base URL is configured.
func (c *Client) Available() bool {
	return c != nil && c.baseURL != ""
}

// request is the body of a FlareSolverr command.
type request struct {
	Cmd        string   `json:"cmd"`
	URL        string   `json:"url,omitempty"`
	MaxTimeout int      `json:"maxTimeout"`
	Cookies    []Cookie `json:"cookies,omitempty"`
}

// Cookie is one FlareSolverr cookie.
type Cookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Domain is rewritten to the exact target host before sending: Chrome's
	// addCookie rejects a leading-dot domain that does not match the page.
	Domain string `json:"domain"`
	Path   string `json:"path,omitempty"`
}

// Solution is the rendered page returned in a successful response.
type Solution struct {
	URL     string `json:"url"`
	Status  int    `json:"status"`
	Cookies []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	} `json:"cookies"`
	UserAgent string `json:"userAgent"`
	Headers   map[string]string
	Title     string
	HTML      string `json:"response"`
}

type apiResponse struct {
	Status    string   `json:"status"`
	Message   string   `json:"message"`
	Solution  Solution `json:"solution"`
	ExpiresAt string   `json:"expiresAt"`
	Version   string   `json:"version"`
}

// Fetch renders a URL and returns the page HTML. Cookies for the URL's host
// are attached from the jar when one is configured.
func (c *Client) Fetch(ctx context.Context, target string) (*Solution, error) {
	if !c.Available() {
		return nil, fmt.Errorf("flaresolverr is not configured")
	}

	req := request{
		Cmd:        "request.get",
		URL:        target,
		MaxTimeout: c.maxTimeout,
	}
	if c.jar != nil {
		cookies, err := c.jar.CookiesForURL(target)
		if err != nil {
			return nil, err
		}
		req.Cookies = cookies
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal flaresolverr request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("flaresolverr request failed: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read flaresolverr response: %w", err)
	}

	var out apiResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("failed to parse flaresolverr response (status %d): %w", resp.StatusCode, err)
	}
	if out.Status != "ok" {
		return nil, fmt.Errorf("flaresolverr error: %s", out.Message)
	}
	return &out.Solution, nil
}

// FetchWithRetry renders a URL, retrying when FlareSolverr cannot solve the
// challenge. The challenge is probabilistic against datacenter IPs, so a
// first failure is often followed by a clean success on the next attempt.
func (c *Client) FetchWithRetry(ctx context.Context, target string, attempts int) (*Solution, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		solution, err := c.Fetch(ctx, target)
		if err == nil {
			return solution, nil
		}
		lastErr = err
		// Do not spend another attempt after the context is done.
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("flaresolverr failed after %d attempts: %w", attempts, lastErr)
}

// hostOf returns the exact host of a URL, used to scope cookies.
func hostOf(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("url has no host: %s", rawURL)
	}
	return parsed.Hostname(), nil
}

// loadFile reads a file and returns an empty slice when it does not exist, so
// a missing cookie jar disables auth instead of failing the tool. A directory
// in place of the file is treated the same way: Docker creates a directory
// when a bind mount points at a path that does not exist on the host.
func loadFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}
