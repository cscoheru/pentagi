package system

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"pentagi/pkg/config"
)

const (
	// defaultHTTPClientTimeout is the fallback timeout when no config is provided.
	defaultHTTPClientTimeout = 10 * time.Minute
	// defaultLLMClientTimeout mirrors config.LLMClientTimeout's envDefault, for callers
	// that run without a config. Kept in step with the 240s default the run budget needs.
	defaultLLMClientTimeout = 4 * time.Minute
)

func getHostname() string {
	hn, err := os.Hostname()
	if err != nil {
		return ""
	}

	return hn
}

func getIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ips = append(ips, addr.String())
		}
	}

	return ips
}

func GetSystemCertPool(cfg *config.Config) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("failed to get system cert pool: %w", err)
	}

	if cfg.ExternalSSLCAPath != "" {
		ca, err := os.ReadFile(cfg.ExternalSSLCAPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read external CA certificate: %w", err)
		}

		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("failed to append external CA certificate to pool")
		}
	}

	return pool, nil
}

// GetHTTPClient returns a client for search engines and other external tools. It keeps the
// generous HTTPClientTimeout on purpose: a single search query routinely takes minutes, and
// sharing a timeout with LLM calls would make one of the two workloads wrong.
func GetHTTPClient(cfg *config.Config) (*http.Client, error) {
	if cfg == nil {
		return getHTTPClient(nil, defaultHTTPClientTimeout)
	}
	return getHTTPClient(cfg, max(time.Duration(cfg.HTTPClientTimeout)*time.Second, 0))
}

// GetLLMClient returns a client for LLM provider API calls. These stream, and http.Client.Timeout
// covers the whole response body read, so this bounds an entire generation. It is deliberately
// separate from GetHTTPClient so a slow search cannot buy an unbounded LLM call and vice versa.
func GetLLMClient(cfg *config.Config) (*http.Client, error) {
	if cfg == nil {
		return getHTTPClient(nil, defaultLLMClientTimeout)
	}
	return getHTTPClient(cfg, max(time.Duration(cfg.LLMClientTimeout)*time.Second, 0))
}

// getHTTPClient builds the transport once for both factories. A cfg of nil yields a client with
// no TLS or proxy customization, matching what GetHTTPClient has always done without config.
func getHTTPClient(cfg *config.Config, timeout time.Duration) (*http.Client, error) {
	if cfg == nil {
		return &http.Client{
			Timeout: timeout,
		}, nil
	}

	rootCAPool, err := GetSystemCertPool(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.ProxyURL != "" {
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					return url.Parse(cfg.ProxyURL)
				},
				TLSClientConfig: &tls.Config{
					RootCAs:            rootCAPool,
					InsecureSkipVerify: cfg.ExternalSSLInsecure,
				},
			},
		}, nil
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            rootCAPool,
				InsecureSkipVerify: cfg.ExternalSSLInsecure,
			},
		},
	}, nil
}
