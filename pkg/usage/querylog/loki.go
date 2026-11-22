package querylog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
)

type lokiQueryRangeResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"` // [unix_ns, log_line]
		} `json:"result"`
	} `json:"data"`
}

var (
	// Mimir query-frontend logfmt query extraction: query="rate(http_requests_total[5m])"
	logfmtQuotedQueryRe = regexp.MustCompile(`(?:^|\s)query="((?:[^"\\]|\\.)*)"`)
	logfmtCallerRe      = regexp.MustCompile(`(?:^|\s)(?:caller|user_agent|org_id)="?([^\s"]+)"?`)
)

// readLokiSource implements the Phase 1.1 Loki-backed query-log source (DESIGN §3.2).
// Queries Loki /loki/api/v1/query_range in bounded time chunks for shipped Prometheus
// query_log_file JSON lines or Mimir query-frontend logs.
func readLokiSource(
	ctx context.Context,
	cfg *config.LokiQueryLogConfig,
	matcher *excludeMatcher,
	cache map[string]*promql.ExtractResult,
	policyWindow time.Duration,
	now time.Time,
	res *Result,
) error {
	if cfg.Query == "" {
		return fmt.Errorf("loki query log source requires non-empty query (LogQL selector)")
	}
	window := cfg.Window.Duration()
	if window <= 0 {
		window = policyWindow
	}
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	chunk := cfg.ChunkSize.Duration()
	if chunk <= 0 {
		chunk = 24 * time.Hour
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = 5000
	}
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	client := &http.Client{Timeout: timeout}
	baseURL := strings.TrimRight(cfg.URL, "/")
	end := now.UTC()
	start := end.Add(-window)

	for cur := start; cur.Before(end); {
		chunkEnd := cur.Add(chunk)
		if chunkEnd.After(end) {
			chunkEnd = end
		}
		if err := fetchLokiChunk(ctx, client, baseURL, cfg, cur, chunkEnd, limit, matcher, cache, res); err != nil {
			return err
		}
		cur = chunkEnd
	}
	return nil
}

func fetchLokiChunk(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	cfg *config.LokiQueryLogConfig,
	start, end time.Time,
	limit int,
	matcher *excludeMatcher,
	cache map[string]*promql.ExtractResult,
	res *Result,
) error {
	params := url.Values{}
	params.Set("query", cfg.Query)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("direction", "forward")

	reqURL := baseURL + "/loki/api/v1/query_range?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", cfg.TenantID)
	}
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("loki query_range request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read loki response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("loki query_range returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed lokiQueryRangeResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode loki response: %w", err)
	}
	if parsed.Status != "success" {
		return fmt.Errorf("loki query_range status=%q", parsed.Status)
	}

	for _, stream := range parsed.Data.Result {
		for _, pair := range stream.Values {
			ns, _ := strconv.ParseInt(pair[0], 10, 64)
			lineTS := time.Unix(0, ns).UTC()
			line := strings.TrimSpace(pair[1])
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "{") {
				// JSON line (Prometheus query_log_file or JSON-formatted Mimir query-frontend)
				ingestLokiJSONLine([]byte(line), lineTS, matcher, cache, res)
			} else {
				// Logfmt line (Mimir query-frontend)
				ingestMimirLogfmtLine(line, lineTS, matcher, cache, res)
			}
		}
	}
	return nil
}

func ingestLokiJSONLine(raw []byte, fallbackTS time.Time, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) {
	before := res.TotalEntries
	ingestJSONLine(raw, "auto", "loki", matcher, cache, res)
	if res.TotalEntries > before && !fallbackTS.IsZero() {
		last := &res.Entries[len(res.Entries)-1]
		if last.Timestamp.IsZero() {
			last.Timestamp = fallbackTS
			if res.EarliestTimestamp.IsZero() || fallbackTS.Before(res.EarliestTimestamp) {
				res.EarliestTimestamp = fallbackTS
			}
			if res.LatestTimestamp.IsZero() || fallbackTS.After(res.LatestTimestamp) {
				res.LatestTimestamp = fallbackTS
			}
		}
	}
}

func ingestMimirLogfmtLine(line string, ts time.Time, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) {
	m := logfmtQuotedQueryRe.FindStringSubmatch(line)
	if len(m) < 2 {
		return
	}
	query, err := strconv.Unquote(`"` + m[1] + `"`)
	if err != nil {
		query = m[1]
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}
	caller := ""
	if cm := logfmtCallerRe.FindStringSubmatch(line); len(cm) > 1 {
		caller = cm[1]
	}
	// Mimir query-frontend logs API user queries (Mimir Ruler bypasses query-frontend).
	origin := metrics.OriginAPIUser
	if matcher.isExcluded(caller, "", query) {
		origin = metrics.OriginExcludedExplorer
	}
	recordEntry(query, ts, origin, "", "", caller, "", "loki:mimir-query-frontend", cache, res)
}
