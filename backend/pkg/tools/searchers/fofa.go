package searchers

// fofa searcher — Chinese asset-mapping primitive via FOFA (fofa.info).
//
// This is the second custom engine of the intel-platform fork. FOFA indexes
// active-scan data (banners, components, ports) on Chinese-hosted targets —
// every IPv4 host, every TLS cert, every exposed service. Where crt.sh yields
// subdomain names from passive CT logs, FOFA yields the *hosts* those names
// resolve to, plus their open ports, software, and banners. Together they
// form a two-layer recon: names (crt.sh) → addresses+services (FOFA).
//
// FOFA requires (email, api_key) auth — register at https://fofa.info →
// 个人中心 → API key. Free tier is 100 results/query; the `quota_free` field
// in the response is exposed in markdown so the operator sees remaining budget.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
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
	defaultFofaLimit   = 25
	maxFofaLimit       = 100 // FOFA free-tier hard cap
	fofaRequestTimeout = 30 * time.Second

	// defaultFofaFields is the FOFA `fields` param. Ordered for human-readable
	// markdown output (host first → IP → port → service identity → geo).
	defaultFofaFields = "host,ip,port,title,domain,server,city,product,os,protocol"
)

// fofa represents the FOFA asset-discovery search primitive.
type fofa struct {
	cfg *config.Config
}

// NewFofa creates a new FOFA search primitive.
func NewFofa(cfg *config.Config) Searcher {
	return &fofa{cfg: cfg}
}

func (s *fofa) Engine() database.SearchengineType {
	return database.SearchengineTypeFofa
}

// IsAvailable is true when both FOFA_EMAIL and FOFA_API_KEY are configured.
// A custom FOFA_API_URL (e.g. a self-hosted FOFA mirror) is honored but optional.
func (s *fofa) IsAvailable() bool {
	return strings.TrimSpace(s.cfg.FofaEmail) != "" && strings.TrimSpace(s.cfg.FofaAPIKey) != ""
}

// Handle processes an asset-mapping search request. The user query is normalized
// into FOFA DSL via buildQuery, sent to FOFA, and the response rendered as a
// markdown table grouped by host.
func (s *fofa) Handle(ctx context.Context, req Request) (string, error) {
	ctx, observation := obs.Observer.NewObservation(ctx)

	dsl, err := s.buildQuery(req.Query)
	if err != nil {
		return "", err
	}

	limit := req.MaxResults
	if limit < 1 || limit > maxFofaLimit {
		limit = defaultFofaLimit
	}

	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"engine":  "fofa",
		"query":   req.Query[:min(len(req.Query), 1000)],
		"dsl":     dsl[:min(len(dsl), 200)],
		"limit":   limit,
	})

	resp, err := s.search(ctx, dsl, limit)
	if err != nil {
		observation.Event(
			langfuse.WithEventName("fofa search error"),
			langfuse.WithEventInput(req.Query),
			langfuse.WithEventStatus(err.Error()),
			langfuse.WithEventLevel(langfuse.ObservationLevelWarning),
			langfuse.WithEventMetadata(langfuse.Metadata{
				"engine": "fofa",
				"query":  req.Query,
				"limit":  limit,
				"error":  err.Error(),
			}),
		)
		obs.LogErrorOrCancel(logger, err, "failed to search in FOFA")
		return "", err
	}

	return resp, nil
}

// buildQuery maps a free-form user query into a FOFA DSL string.
//
// Resolution order:
//  1. If the query already looks like FOFA DSL (contains "=" AND quote), it
//     is accepted ONLY when the field name is in fofaAllowedFields — this
//     guards against prompt-injection where the LLM constructs an exotic or
//     adversarial DSL string.
//  2. A bare IPv4 (net.ParseIP) or IPv4 CIDR (net.ParseCIDR) is wrapped as
//     ip="...". This check runs BEFORE the path/port strip because the strip
//     would otherwise eat the CIDR's slash.
//  3. A DNS hostname (any number of labels, with a valid public TLD) is
//     wrapped as domain="..." so every issued subdomain is returned. The
//     multi-label suffix check covers both "example.com" and "example.co.uk"
//     uniformly — both want "matches every subdomain", which is what
//     domain= does. host= remains reachable via the verbatim DSL path when
//     the LLM really wants an exact hostname match.
//  4. Anything else (single word, ambiguous, junk) falls back to title="..." —
//     FOFA scans page titles so this always returns SOMETHING rather than
//     failing the call. Multi-word queries (e.g., "Apache Struts") are
//     preserved verbatim inside the title DSL.
//
// Only tabs/newlines and '@' are hard-rejected. Spaces are preserved for
// keyword-style queries.
func (s *fofa) buildQuery(query string) (string, error) {
	q := strings.TrimSpace(query)
	q = strings.TrimPrefix(q, "http://")
	q = strings.TrimPrefix(q, "https://")
	q = strings.TrimSuffix(q, "/")

	if q == "" {
		return "", Fatal(fmt.Errorf("fofa: 'query' is required"))
	}

	// Already DSL? Whitelist-validate the field name. Keep the original
	// casing of the value (FOFA field names are case-sensitive; values
	// are case-insensitive but preserving user intent is cleaner).
	if strings.Contains(q, "=") && strings.Contains(q, "\"") {
		return s.validateDSL(q)
	}

	// From here on, the input is not DSL — lowercase for classification.
	q = strings.ToLower(q)

	// Reject only the unambiguous junk. Spaces stay — multi-word queries
	// become title="..." which is valid FOFA DSL.
	if strings.ContainsAny(q, "\t\r\n@") {
		return "", Fatal(fmt.Errorf("fofa: query %q contains disallowed characters", clipQuery(q)))
	}

	// IPv4 CIDR — MUST run before the path/port strip, otherwise the "/24"
	// suffix gets eaten.
	if _, _, err := net.ParseCIDR(q); err == nil {
		return fmt.Sprintf("ip=%q", q), nil
	}
	// Bare IPv4.
	if net.ParseIP(q) != nil {
		return fmt.Sprintf("ip=%q", q), nil
	}

	// Strip an accidental path / port suffix (e.g., "example.com:8080/foo" →
	// "example.com"). This is a no-op for IPs and CIDRs above.
	if i := strings.IndexAny(q, "/:"); i > 0 {
		q = q[:i]
	}
	if q == "" {
		return "", Fatal(fmt.Errorf("fofa: 'query' is required after path/port strip"))
	}

	// DNS hostname: any number of labels, with a valid public TLD.
	if isDNSHostname(q) {
		return fmt.Sprintf("domain=%q", q), nil
	}

	// Last resort: keyword/title search. FOFA always has SOME result.
	return fmt.Sprintf("title=%q", q), nil
}

// validateDSL accepts a verbatim FOFA DSL string only when its leading field
// name is in the strict whitelist. This blocks prompt injection via crafted
// DSL (e.g., field names that smuggle base64 payloads or unsupported
// operators).
func (s *fofa) validateDSL(q string) (string, error) {
	i := strings.Index(q, "=")
	if i <= 0 {
		return "", Fatal(fmt.Errorf("fofa: malformed DSL %q (no leading field)", clipQuery(q)))
	}
	field := strings.TrimSpace(q[:i])
	if _, ok := fofaAllowedFields[field]; !ok {
		return "", Fatal(fmt.Errorf("fofa: DSL field %q is not in the whitelist (use one of: %s)",
			field, fofaAllowedFieldsKeys()))
	}
	return q, nil
}

// fofaAllowedFields enumerates the FOFA field names the LLM is permitted to
// invoke directly. Anything outside this set is rejected to keep the searcher
// from executing arbitrary FOFA operators. The map form gives O(1) lookup
// and a stable enumeration order for error messages.
var fofaAllowedFields = map[string]struct{}{
	"domain": {}, "ip": {}, "host": {}, "title": {}, "cert": {}, "port": {}, "protocol": {},
	"product": {}, "server": {}, "os": {}, "banner": {}, "city": {}, "country": {},
	"header": {}, "body": {}, "jarm": {}, "asn": {}, "org": {}, "base_protocol": {},
	"is_domain": {}, "is_ipv4": {}, "icon_hash": {}, "fid": {}, "sitemap": {},
}

func fofaAllowedFieldsKeys() string {
	out := make([]string, 0, len(fofaAllowedFields))
	for k := range fofaAllowedFields {
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

// dnsLabelRE enforces the standard DNS label charset: 1-63 chars, alnum +
// hyphen, cannot start or end with hyphen.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// tldRE accepts either:
//   - a 2+ character alphabetic TLD (com, org, cn, io, …), or
//   - a Punycode (IDN) TLD starting with "xn--" (e.g., xn--0zwm56d for 中国).
// One-letter "TLDs" are deliberately rejected — public DNS never assigns
// them and FOFA has nothing to match against.
var tldRE = regexp.MustCompile(`^[a-z]{2,}$|^xn--[a-z0-9]{2,}$`)

// isDNSHostname reports whether q parses as a valid DNS hostname. It rejects
// bare IPv4 strings (handled earlier by net.ParseIP), pure-numeric labels,
// labels that violate DNS charset, and 1-label inputs.
func isDNSHostname(q string) bool {
	if !strings.Contains(q, ".") {
		return false
	}
	labels := strings.Split(q, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !dnsLabelRE.MatchString(l) {
			return false
		}
	}
	return tldRE.MatchString(labels[len(labels)-1])
}

// search calls the FOFA /api/v1/search/all endpoint and renders markdown.
func (s *fofa) search(ctx context.Context, dsl string, limit int) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.cfg.FofaAPIURL), "/")
	if base == "" {
		base = "https://fofa.info"
	}

	qbase64 := base64.StdEncoding.EncodeToString([]byte(dsl))

	params := url.Values{}
	params.Set("qbase64", qbase64)
	params.Set("email", s.cfg.FofaEmail)
	params.Set("key", s.cfg.FofaAPIKey)
	params.Set("size", fmt.Sprintf("%d", limit))
	params.Set("page", "1")
	params.Set("fields", defaultFofaFields)

	u := base + "/api/v1/search/all?" + params.Encode()

	client, err := system.GetHTTPClient(s.cfg)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create http client: %w", err))
	}
	client.Timeout = fofaRequestTimeout

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create request: %w", err))
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", Retryable(fmt.Errorf("fofa request failed: %w", err), 5*time.Second)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", Retryable(fmt.Errorf("fofa rate limit (HTTP 429)"), 30*time.Second)
	case resp.StatusCode >= 500:
		return "", Retryable(fmt.Errorf("fofa server error (HTTP %d)", resp.StatusCode), 10*time.Second)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return "", Fatal(fmt.Errorf("fofa auth failed (HTTP %d) — check FOFA_EMAIL and FOFA_API_KEY", resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", Fatal(fmt.Errorf("fofa unexpected status: HTTP %d body=%q", resp.StatusCode, strings.TrimSpace(string(body))))
	}

	var raw fofaResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 10*1024*1024)).Decode(&raw); err != nil {
		return "", Fatal(fmt.Errorf("failed to decode fofa response: %w", err))
	}

	if !raw.IsSuccess() {
		// FOFA returns {"error": true, "errmsg": "..."} on auth/quota/query errors.
		return "", Fatal(fmt.Errorf("fofa API error: %s", raw.Errmsg))
	}

	return s.format(dsl, &raw, limit), nil
}

// fofaResponse mirrors the JSON envelope FOFA returns. `Results` is a 2D
// array because each row maps to the requested `fields` list.
type fofaResponse struct {
	Error     bool           `json:"error"`
	Errmsg    string         `json:"errmsg"`
	Query     string         `json:"query"`
	Size      int            `json:"size"`
	Total     int            `json:"total"`     // total matching records in FOFA
	Mode      string         `json:"mode"`      // "extended" with paid plan, "normal" with free
	Page      int            `json:"page"`
	Results   [][]string     `json:"results"`   // rows × fields
	Fields    []string       `json:"fields"`    // echoed from request
	QuotaFree interface{}    `json:"quota_free,omitempty"` // remaining free quota (free tier only)
}

func (r *fofaResponse) IsSuccess() bool {
	return r != nil && !r.Error && r.Errmsg == ""
}

// format renders FOFA results as a markdown table grouped by host. The first
// row of each column is the field name from `defaultFofaFields`.
func (s *fofa) format(dsl string, resp *fofaResponse, limit int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## FOFA asset mapping results for `%s`\n\n", clipQuery(dsl)))
	fmt.Fprintf(&sb, "**Total:** %d matching record(s) in FOFA  \n", resp.Total)
	fmt.Fprintf(&sb, "**Mode:** %s  \n", orDefault(resp.Mode, "normal"))
	if resp.QuotaFree != nil {
		fmt.Fprintf(&sb, "**Free quota left:** %v  \n", resp.QuotaFree)
	}
	fmt.Fprintf(&sb, "**Showing:** up to %d  \n\n", limit)

	if len(resp.Results) == 0 {
		sb.WriteString("No assets matched the query.\n")
		return sb.String()
	}

	// Determine column order: prefer the explicit `fields` echo from FOFA, fall
	// back to our default list. This keeps rendering stable across FOFA API
	// version drift.
	headers := resp.Fields
	if len(headers) == 0 {
		headers = strings.Split(defaultFofaFields, ",")
	}

	// Markdown header row.
	sb.WriteString("| " + strings.Join(headers, " | ") + " |\n")
	sb.WriteString("|" + strings.Repeat(" --- |", len(headers)) + "\n")

	shown := 0
	for _, row := range resp.Results {
		if shown >= limit {
			break
		}
		cells := make([]string, len(headers))
		for i := range headers {
			if i < len(row) {
				cells[i] = escapeCell(row[i])
			}
		}
		sb.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		shown++
	}

	if resp.Total > shown {
		fmt.Fprintf(&sb, "\n**⚠️ Note:** %d additional matches truncated (FOFA free tier returns up to %d per query).\n",
			resp.Total-shown, maxFofaLimit)
	}

	return sb.String()
}

// escapeCell neutralizes pipes inside a FOFA result cell so they don't break
// the markdown table.
func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
