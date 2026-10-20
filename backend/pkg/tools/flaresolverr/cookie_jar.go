package flaresolverr

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// CookieJar is a server-side map of host to the cookies that establish a
// logged-in session. It is loaded from a file outside tool call arguments, so
// credentials never pass through the model.
//
// File shape:
//
//	{
//	  "tieba.baidu.com": [{"name": "BDUSS", "value": "..."}, ...],
//	  "www.zhihu.com":    [{"name": "z_c0", "value": "..."}]
//	}
type CookieJar struct {
	mu      sync.RWMutex
	cookies map[string][]Cookie
}

// NewCookieJar returns an empty jar.
func NewCookieJar() *CookieJar {
	return &CookieJar{cookies: make(map[string][]Cookie)}
}

// LoadCookieJar reads a jar from path. A missing file yields an empty jar so
// that the fallback is simply unauthenticated.
func LoadCookieJar(path string) (*CookieJar, error) {
	jar := NewCookieJar()
	if path == "" {
		return jar, nil
	}
	data, err := loadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cookie jar %q: %w", path, err)
	}
	if len(data) == 0 {
		return jar, nil
	}

	var raw map[string][]struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse cookie jar %q: %w", path, err)
	}

	for host, cookies := range raw {
		for _, c := range cookies {
			if c.Name == "" || c.Value == "" {
				continue
			}
			path := c.Path
			if path == "" {
				path = "/"
			}
			jar.cookies[host] = append(jar.cookies[host], Cookie{
				Name:   c.Name,
				Value:  c.Value,
				Domain: host, // always scoped to the exact host
				Path:   path,
			})
		}
	}
	return jar, nil
}

// CookiesForURL returns the cookies configured for the URL's host.
func (j *CookieJar) CookiesForURL(target string) ([]Cookie, error) {
	if j == nil {
		return nil, nil
	}
	host, err := hostOf(target)
	if err != nil {
		return nil, err
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	cookies := j.cookies[host]
	if len(cookies) == 0 {
		return nil, nil
	}
	out := make([]Cookie, len(cookies))
	copy(out, cookies)
	return out, nil
}

// Set replaces the cookies for a host. It exists for the fslogin bootstrap
// helper, which writes newly obtained login cookies into the jar.
func (j *CookieJar) Set(host string, cookies []Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	scoped := make([]Cookie, 0, len(cookies))
	for _, c := range cookies {
		c.Domain = host
		if c.Path == "" {
			c.Path = "/"
		}
		scoped = append(scoped, c)
	}
	j.cookies[host] = scoped
}

// Save writes the jar back to path with 0600 permissions.
func (j *CookieJar) Save(path string) error {
	if path == "" {
		return fmt.Errorf("cookie jar path is empty")
	}
	j.mu.RLock()
	defer j.mu.RUnlock()

	export := make(map[string][]Cookie, len(j.cookies))
	for host, cookies := range j.cookies {
		export[host] = cookies
	}
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
