package searchers

// shodan_internetdb searcher — Shodan's free InternetDB API (zero key, zero quota).
//
// InternetDB is Shodan's open dataset: per-IP, it returns open ports, CPE product
// fingerprints, hostnames, "tags" (e.g. eol-product, vpn), and a list of CVEs
// Shodan associates with the host's software. Public endpoint, JSON, no auth, no
// rate limit beyond a soft abuse threshold. It's the only first-class OSINT
// primitive that gives an LLM structured vulnerability intelligence on a target IP
// without any paid API key.
//
// The endpoint needs no API key, so IsAvailable() is always true; SHODAN_INTERNETDB_API_URL
// lets operators point at a self-hosted mirror if Shodan's CDN is unreachable.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
	obs "pentagi/pkg/observability"
	"pentagi/pkg/observability/langfuse"
	"pentagi/pkg/system"

	"github.com/sirupsen/logrus"
)

const (
	defaultShodanInternetDBBase = "https://internetdb.shodan.io"
	shodanInternetDBTimeout     = 30 * time.Second
	maxShodanInternetDBBytes    = 4 * 1024 * 1024 // 4 MiB — InternetDB responses are small but cap for safety
)

// shodanInternetDBResponse is the JSON shape InternetDB returns for one IP.
type shodanInternetDBResponse struct {
	IP        string   `json:"ip"`
	Ports     []int    `json:"ports"`
	CPEs      []string `json:"cpes"`
	Hostnames []string `json:"hostnames"`
	Tags      []string `json:"tags"`
	Vulns     []string `json:"vulns"`
}

// shodanInternetDB represents the Shodan InternetDB IP-intelligence search primitive.
type shodanInternetDB struct {
	cfg *config.Config
}

// NewShodanInternetDB creates a new Shodan InternetDB search primitive.
func NewShodanInternetDB(cfg *config.Config) Searcher {
	return &shodanInternetDB{cfg: cfg}
}

func (s *shodanInternetDB) Engine() database.SearchengineType {
	return database.SearchengineTypeShodanInternetDB
}

// IsAvailable is always true: InternetDB is a free public API with no key. A custom
// SHODAN_INTERNETDB_API_URL mirror is honored but not required.
func (s *shodanInternetDB) IsAvailable() bool {
	return true
}

// Handle processes an IP-intelligence search request from the orchestrator. The
// query must parse as an IPv4 or IPv6 address (the orchestrator already pre-parses;
// we re-validate defensively because the LLM may emit a hostname that LOOKS like
// an IP, e.g. "1.2.3" or "::1.local"). A bare hostname is rejected — for hostname
// resolution, callers should run crt.sh or searxng first to discover IPs.
func (s *shodanInternetDB) Handle(ctx context.Context, req Request) (string, error) {
	ctx, observation := obs.Observer.NewObservation(ctx)

	ip, err := s.parseIP(req.Query)
	if err != nil {
		return "", err
	}

	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"engine": "shodan_internetdb",
		"query":  req.Query[:min(len(req.Query), 1000)],
		"ip":     ip,
	})

	result, err := s.search(ctx, ip)
	if err != nil {
		observation.Event(
			langfuse.WithEventName("shodan_internetdb search error"),
			langfuse.WithEventInput(req.Query),
			langfuse.WithEventStatus(err.Error()),
		)
		obs.LogErrorOrCancel(logger, err, "failed to search in Shodan InternetDB")
		return "", err
	}

	observation.Event(
		langfuse.WithEventName("shodan_internetdb search ok"),
		langfuse.WithEventInput(req.Query),
		langfuse.WithEventOutput(result),
	)

	return result, nil
}

// parseIP extracts a valid IPv4 or IPv6 address from the (possibly noisy) LLM query.
// The orchestrator guarantees non-empty input; we accept bare IPs, IPs wrapped in
// brackets, and URLs where the IP is the host portion. Anything else is a Fatal
// error — InternetDB has no fallback semantics for a non-IP query.
func (s *shodanInternetDB) parseIP(query string) (string, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", Fatal(fmt.Errorf("shodan_internetdb: 'query' is required"))
	}

	// Strip URL wrapping (http://1.2.3.4/, https://[::1]:8080/path).
	if strings.Contains(q, "://") {
		if u, err := url.Parse(q); err == nil && u.Host != "" {
			q = u.Host
		}
	}
	// Trim any trailing :port or /path that survived.
	if i := strings.LastIndex(q, "/"); i >= 0 {
		q = q[:i]
	}
	if strings.HasPrefix(q, "[") {
		// IPv6 with brackets: [::1]:8080
		if i := strings.LastIndex(q, "]"); i > 0 {
			q = q[1:i]
		}
	} else if strings.Count(q, ":") == 1 {
		// IPv4 with port: 1.2.3.4:8080
		if i := strings.LastIndex(q, ":"); i >= 0 {
			q = q[:i]
		}
	}
	// IPv6 with zone id (e.g. fe80::1%eth0) — net.ParseIP rejects these; strip zone.
	if i := strings.Index(q, "%"); i >= 0 {
		q = q[:i]
	}

	ip := net.ParseIP(q)
	if ip == nil {
		return "", Fatal(fmt.Errorf("shodan_internetdb: query %q is not a valid IPv4 or IPv6 address", q))
	}
	return ip.String(), nil
}

// search performs the InternetDB lookup and renders the structured response as
// markdown the LLM can ingest.
func (s *shodanInternetDB) search(ctx context.Context, ip string) (string, error) {
	base := s.cfg.ShodanInternetDBAPIURL
	if base == "" {
		base = defaultShodanInternetDBBase
	}
	// Defensive: trim trailing slash so URL concatenation is unambiguous.
	base = strings.TrimRight(base, "/")

	u := fmt.Sprintf("%s/%s", base, ip)

	client, err := system.GetHTTPClient(s.cfg)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create http client: %w", err))
	}
	client.Timeout = shodanInternetDBTimeout

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create request: %w", err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		// Transport errors (timeouts, resets) are worth one retry at the orchestrator level.
		return "", Retryable(fmt.Errorf("shodan_internetdb request failed: %w", err), 5*time.Second)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", Retryable(fmt.Errorf("shodan_internetdb rate limit (HTTP 429)"), 30*time.Second)
	case resp.StatusCode >= 500:
		return "", Retryable(fmt.Errorf("shodan_internetdb server error (HTTP %d)", resp.StatusCode), 10*time.Second)
	case resp.StatusCode == http.StatusNotFound:
		// 404 means InternetDB has no record for this IP. That's a real (empty)
		// answer, not an error — surface it so the agent can move on.
		return s.format(ip, shodanInternetDBResponse{IP: ip}), nil
	case resp.StatusCode != http.StatusOK:
		return "", Fatal(fmt.Errorf("shodan_internetdb unexpected status: HTTP %d", resp.StatusCode))
	}

	var body shodanInternetDBResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxShodanInternetDBBytes)).Decode(&body); err != nil {
		return "", Fatal(fmt.Errorf("failed to decode shodan_internetdb response: %w", err))
	}
	// Be defensive about an empty body — treat as 404-equivalent.
	if body.IP == "" {
		body.IP = ip
	}

	return s.format(ip, body), nil
}

// format renders the structured response as markdown the LLM can ingest. The shape
// is deliberately compact: each section is present iff it has data, so the model
// can pattern-match on section headers.
func (s *shodanInternetDB) format(ip string, r shodanInternetDBResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Shodan InternetDB: %s\n\n", ip)

	empty := len(r.Ports) == 0 && len(r.CPEs) == 0 && len(r.Hostnames) == 0 && len(r.Tags) == 0 && len(r.Vulns) == 0
	if empty {
		b.WriteString("No records on file. The IP has no observed open ports, software fingerprints, hostnames, tags, or known CVEs in Shodan InternetDB.\n")
		return b.String()
	}

	if len(r.Ports) > 0 {
		fmt.Fprintf(&b, "### Open Ports (%d)\n\n", len(r.Ports))
		for _, p := range r.Ports {
			fmt.Fprintf(&b, "- %d\n", p)
		}
		b.WriteString("\n")
	}

	if len(r.CPEs) > 0 {
		fmt.Fprintf(&b, "### Software / CPEs (%d)\n\n", len(r.CPEs))
		for _, c := range r.CPEs {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		b.WriteString("\n")
	}

	if len(r.Hostnames) > 0 {
		fmt.Fprintf(&b, "### Hostnames (%d)\n\n", len(r.Hostnames))
		for _, h := range r.Hostnames {
			fmt.Fprintf(&b, "- %s\n", h)
		}
		b.WriteString("\n")
	}

	if len(r.Tags) > 0 {
		fmt.Fprintf(&b, "### Tags (%d)\n\n", len(r.Tags))
		for _, t := range r.Tags {
			fmt.Fprintf(&b, "- %s\n", t)
		}
		b.WriteString("\n")
	}

	if len(r.Vulns) > 0 {
		fmt.Fprintf(&b, "### Known CVEs (%d)\n\n", len(r.Vulns))
		for _, v := range r.Vulns {
			fmt.Fprintf(&b, "- %s\n", v)
		}
		b.WriteString("\n")
	}

	return b.String()
}
