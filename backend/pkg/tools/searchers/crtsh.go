package searchers

// crtsh searcher — Certificate Transparency discovery via crt.sh.
//
// This is the first custom engine of the intel-platform fork. crt.sh aggregates
// every certificate logged to public CT logs, which makes it a first-class OSINT
// primitive: one query on a root domain yields every issued subdomain, wildcard
// cert and issuing CA — the classic subdomain-enumeration starting point that no
// general web engine provides.
//
// The public endpoint needs no API key, so IsAvailable() is always true; CRTSH_API_URL
// lets operators point at a mirror (crt.sh itself is rate-limited at times).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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
	defaultCrtshLimit   = 25
	maxCrtshLimit       = 100
	crtshRequestTimeout = 90 * time.Second // crt.sh is notoriously slow under load
)

// crtshEntry is one row of the crt.sh JSON output. Timestamps arrive in several
// layouts depending on the log, so they are parsed best-effort for sorting only.
type crtshEntry struct {
	IssuerCaID     int    `json:"issuer_ca_id"`
	IssuerName     string `json:"issuer_name"`
	CommonName     string `json:"common_name"`
	NameValue      string `json:"name_value"` // newline-separated SAN list
	ID             int64  `json:"id"`
	EntryTimestamp string `json:"entry_timestamp"`
	NotBefore      string `json:"not_before"`
	NotAfter       string `json:"not_after"`
	SerialNumber   string `json:"serial_number"`
}

// crtsh represents the crt.sh certificate-transparency search primitive.
type crtsh struct {
	cfg *config.Config
}

// NewCrtsh creates a new crt.sh search primitive.
func NewCrtsh(cfg *config.Config) Searcher {
	return &crtsh{cfg: cfg}
}

func (s *crtsh) Engine() database.SearchengineType {
	return database.SearchengineTypeCrtsh
}

// IsAvailable is always true: crt.sh is a free public API with no key. A custom
// CRTSH_API_URL mirror is honored but not required.
func (s *crtsh) IsAvailable() bool {
	return true
}

// Handle processes a certificate-transparency search request from the orchestrator.
// The query must be a domain (a wildcard "*" or "%" prefix is accepted verbatim);
// a bare domain is automatically prefixed with "%." so subdomains are included.
func (s *crtsh) Handle(ctx context.Context, req Request) (string, error) {
	ctx, observation := obs.Observer.NewObservation(ctx)

	domain, err := s.buildQuery(req.Query)
	if err != nil {
		return "", err
	}

	limit := req.MaxResults
	if limit < 1 || limit > maxCrtshLimit {
		limit = defaultCrtshLimit
	}

	logger := logrus.WithContext(ctx).WithFields(logrus.Fields{
		"engine": "crtsh",
		"query":  req.Query[:min(len(req.Query), 1000)],
		"limit":  limit,
	})

	result, err := s.search(ctx, domain, limit)
	if err != nil {
		observation.Event(
			langfuse.WithEventName("crtsh search error"),
			langfuse.WithEventInput(req.Query),
			langfuse.WithEventStatus(err.Error()),
			langfuse.WithEventLevel(langfuse.ObservationLevelWarning),
			langfuse.WithEventMetadata(langfuse.Metadata{
				"engine": "crtsh",
				"query":  req.Query,
				"limit":  limit,
				"error":  err.Error(),
			}),
		)

		obs.LogErrorOrCancel(logger, err, "failed to search in crt.sh")
		return "", err
	}

	return result, nil
}

// buildQuery normalizes the user query into a crt.sh match expression. Bare
// domains get a "%." (any-subdomain) prefix; explicit wildcards are honored.
func (s *crtsh) buildQuery(query string) (string, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	q = strings.TrimPrefix(q, "http://")
	q = strings.TrimPrefix(q, "https://")
	q = strings.TrimSuffix(q, "/")

	if q == "" {
		return "", Fatal(fmt.Errorf("crtsh: 'query' is required and must be a domain name"))
	}

	// Honor explicit wildcards ("*.example.com", "%.example.com") verbatim.
	if strings.ContainsAny(q, "*%") {
		return strings.ReplaceAll(q, "*", "%"), nil
	}

	// Reject obvious non-domains (no dot, or whitespace inside).
	if !strings.Contains(q, ".") || strings.ContainsAny(q, " \t") {
		return "", Fatal(fmt.Errorf("crtsh: query %q does not look like a domain name (expected e.g. 'example.com' or '*.example.com')", clipQuery(q)))
	}

	// Bare root domain → include every subdomain.
	return "%." + q, nil
}

// clipQuery keeps error messages from echoing arbitrarily long input.
func clipQuery(q string) string {
	if len(q) > 100 {
		return q[:100] + "..."
	}
	return q
}

// search calls the crt.sh JSON API and returns a formatted markdown result string.
func (s *crtsh) search(ctx context.Context, domain string, limit int) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.cfg.CrtshAPIURL), "/")
	if base == "" {
		base = "https://crt.sh"
	}

	u := fmt.Sprintf("%s/?q=%s&output=json&limit=%d", base, url.QueryEscape(domain), limit)

	client, err := system.GetHTTPClient(s.cfg)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create http client: %w", err))
	}
	client.Timeout = crtshRequestTimeout

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", Fatal(fmt.Errorf("failed to create request: %w", err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		// Transport errors (timeouts, resets) are worth one retry at the orchestrator level.
		return "", Retryable(fmt.Errorf("crtsh request failed: %w", err), 5*time.Second)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", Retryable(fmt.Errorf("crtsh rate limit (HTTP 429)"), 30*time.Second)
	}
	if resp.StatusCode >= 500 {
		return "", Retryable(fmt.Errorf("crtsh server error (HTTP %d)", resp.StatusCode), 10*time.Second)
	}
	if resp.StatusCode != http.StatusOK {
		return "", Fatal(fmt.Errorf("crtsh unexpected status: HTTP %d", resp.StatusCode))
	}

	var entries []crtshEntry
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 10*1024*1024)).Decode(&entries); err != nil {
		return "", Fatal(fmt.Errorf("failed to decode crt.sh response: %w", err))
	}

	return s.format(domain, entries), nil
}

// crtshTime parses the several timestamp layouts crt.sh emits, best-effort.
func crtshTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// format deduplicates entries (crt.sh returns one row per CT log occurrence),
// keeps the newest occurrence per unique cert, and renders markdown.
func (s *crtsh) format(domain string, entries []crtshEntry) string {
	type certRow struct {
		e       crtshEntry
		fetchTS time.Time
	}

	newest := make(map[string]certRow)
	for _, e := range entries {
		key := strings.ToLower(e.CommonName) + "|" + strings.ToLower(e.NameValue) + "|" + strings.ToLower(e.SerialNumber)
		ts := crtshTime(e.EntryTimestamp)
		if prev, ok := newest[key]; !ok || ts.After(prev.fetchTS) {
			newest[key] = certRow{e: e, fetchTS: ts}
		}
	}

	rows := make([]certRow, 0, len(newest))
	for _, r := range newest {
		rows = append(rows, r)
	}
	// Newest first.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].fetchTS.After(rows[j].fetchTS) })

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Certificate Transparency results for `%s` (crt.sh)\n\n", domain))

	if len(rows) == 0 {
		sb.WriteString("No certificates were found for the given domain.\n")
		return sb.String()
	}

	shown := 0
	truncated := false
	currentSize := len(sb.String())

	for _, r := range rows {
		sans := strings.Fields(r.e.NameValue)
		uniqueSANs := make([]string, 0, len(sans))
		seen := make(map[string]struct{}, len(sans))
		for _, san := range sans {
			l := strings.ToLower(san)
			if _, dup := seen[l]; !dup {
				seen[l] = struct{}{}
				uniqueSANs = append(uniqueSANs, san)
			}
		}

		var item strings.Builder
		item.WriteString(fmt.Sprintf("### %s\n\n", r.e.CommonName))
		if len(uniqueSANs) > 1 {
			item.WriteString(fmt.Sprintf("**SANs (%d):** %s  \n", len(uniqueSANs), strings.Join(uniqueSANs, ", ")))
		}
		if issuer := shortIssuer(r.e.IssuerName); issuer != "" {
			item.WriteString(fmt.Sprintf("**Issuer:** %s  \n", issuer))
		}
		if r.e.NotBefore != "" && r.e.NotAfter != "" {
			item.WriteString(fmt.Sprintf("**Validity:** %s → %s  \n", r.e.NotBefore, r.e.NotAfter))
		}
		if r.e.SerialNumber != "" {
			item.WriteString(fmt.Sprintf("**Serial:** %s  \n", r.e.SerialNumber))
		}
		if r.e.ID != 0 {
			item.WriteString(fmt.Sprintf("**Details:** https://crt.sh/?id=%d  \n", r.e.ID))
		}
		item.WriteString("\n---\n\n")

		content := item.String()
		if currentSize+len(content) > maxTotalResultSize-truncationMsgBuffer {
			truncated = true
			break
		}
		sb.WriteString(content)
		currentSize += len(content)
		shown++
	}

	if truncated {
		sb.WriteString(fmt.Sprintf(
			"\n**⚠️ Note:** Results truncated after %d unique certificates due to %d bytes size limit (of %d unique certs).\n",
			shown, maxTotalResultSize, len(rows),
		))
	}

	return sb.String()
}

// shortIssuer reduces a full DN ("C=US, O=Let's Encrypt, CN=R3") to "Let's Encrypt R3".
func shortIssuer(dn string) string {
	if dn == "" {
		return ""
	}
	parts := strings.Split(dn, ",")
	var org, cn string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasPrefix(p, "O="):
			org = strings.TrimPrefix(p, "O=")
		case strings.HasPrefix(p, "CN="):
			cn = strings.TrimPrefix(p, "CN=")
		}
	}
	switch {
	case org != "" && cn != "":
		return org + " " + cn
	case cn != "":
		return cn
	default:
		return org
	}
}
