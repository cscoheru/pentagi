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
// TODO(user contribution, 5-10 lines): implement query normalization.
//
// The user query is one of:
//   - Bare root domain:        "example.com"             → domain="example.com"
//   - Wildcard subdomain:      "*.example.com"           → domain="*.example.com"
//   - IPv4 address:            "1.2.3.4"                 → ip="1.2.3.4"
//   - IPv4 CIDR:               "1.2.3.0/24"              → ip="1.2.3.0/24"
//   - Bare title keyword:      "Kubelet"                 → title="Kubelet"
//   - Already FOFA DSL:        'host="api.example.com"'  → return verbatim
//   - Component / banner:      'product="Nginx"'         → return verbatim
//
// Strategy: detect via regex whether the input is already a FOFA DSL
// (contains '=' and quotes), otherwise classify by structure (contains '/'
// → CIDR; matches ipv4 → ip; contains '.' but not '=' → domain; else title).
// Strip http(s):// prefix; trim trailing slashes; lowercase. Return only the
// DSL fragment (no leading "q=" — base64 happens in search()).
func (s *fofa) buildQuery(query string) (string, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	q = strings.TrimPrefix(q, "http://")
	q = strings.TrimPrefix(q, "https://")
	q = strings.TrimSuffix(q, "/")

	if q == "" {
		return "", Fatal(fmt.Errorf("fofa: 'query' is required"))
	}

	// TODO: replace this stub with the real classifier described above.
	// For now we pass through verbatim — better than failing, and lets the
	// operator smoke-test the rest of the pipeline with explicit FOFA DSL.
	return q, nil
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
