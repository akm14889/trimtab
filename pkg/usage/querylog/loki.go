package querylog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
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

// readLokiSource implements the Loki-backed query-log source (DESIGN §3.2) with unified auth,
// forward pagination within chunks, Mimir param_query/param_match[] parsing, and self-query exclusion.
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
	if window > res.RequestedWindow {
		res.RequestedWindow = window
	}
	chunk := cfg.ChunkSize.Duration()
	if chunk <= 0 {
		chunk = 24 * time.Hour
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = 5000
	}
	maxPages := cfg.MaxPagesPerChunk
	if maxPages <= 0 {
		maxPages = 100
	}
	timeout := cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	auth, err := cfg.EffectiveAuth()
	if err != nil {
		return err
	}
	client, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.URL,
		Timeout:     timeout,
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		return fmt.Errorf("loki client: %w", err)
	}
	res.Warnings = append(res.Warnings, client.Warnings()...)

	end := now.UTC()
	start := end.Add(-window)

	for cur := start; cur.Before(end); {
		chunkEnd := cur.Add(chunk)
		if chunkEnd.After(end) {
			chunkEnd = end
		}
		if err := fetchLokiChunk(ctx, client, cfg, cur, chunkEnd, limit, maxPages, matcher, cache, res); err != nil {
			return err
		}
		cur = chunkEnd
	}
	return nil
}

func fetchLokiChunk(
	ctx context.Context,
	client *httpclient.Client,
	cfg *config.LokiQueryLogConfig,
	start, end time.Time,
	limit, maxPages int,
	matcher *excludeMatcher,
	cache map[string]*promql.ExtractResult,
	res *Result,
) error {
	curStartNs := start.UnixNano()
	endNs := end.UnixNano()

	for page := 0; page < maxPages; page++ {
		params := url.Values{}
		params.Set("query", cfg.Query)
		params.Set("start", strconv.FormatInt(curStartNs, 10))
		params.Set("end", strconv.FormatInt(endNs, 10))
		params.Set("limit", strconv.Itoa(limit))
		params.Set("direction", "forward")

		body, err := client.Get(ctx, "/loki/api/v1/query_range?"+params.Encode())
		if err != nil {
			return fmt.Errorf("loki query_range failed: %w", err)
		}

		var parsed lokiQueryRangeResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return fmt.Errorf("decode loki response: %w", err)
		}
		if parsed.Status != "success" {
			return fmt.Errorf("loki query_range status=%q", parsed.Status)
		}

		pageCount := 0
		var maxTS int64
		for _, stream := range parsed.Data.Result {
			for _, pair := range stream.Values {
				pageCount++
				ns, _ := strconv.ParseInt(pair[0], 10, 64)
				if ns > maxTS {
					maxTS = ns
				}
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

		if pageCount < limit {
			return nil
		}
		// Tie-guard: if all returned lines share or precede curStartNs, pagination cannot advance safely.
		if maxTS <= curStartNs {
			return fmt.Errorf("loki pagination tie-guard tripped at ns=%d (returned %d >= limit %d with non-advancing timestamp)", curStartNs, pageCount, limit)
		}
		curStartNs = maxTS + 1
		if curStartNs > endNs {
			return nil
		}
	}

	return fmt.Errorf("loki chunk [%s..%s] exceeded maxPagesPerChunk=%d at limit=%d (refusing silent truncation)",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), maxPages, limit)
}

func ingestLokiJSONLine(raw []byte, fallbackTS time.Time, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) {
	ingestJSONLine(raw, "auto", "loki", fallbackTS, matcher, cache, res)
}

func ingestMimirLogfmtLine(line string, ts time.Time, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) {
	fields := parseLogfmtFields(line)
	query := strings.TrimSpace(firstNonEmpty(
		fields["param_query"],
		fields["query"],
		fields["param_match[]"],
		fields["match[]"],
	))
	if query == "" {
		return
	}

	userAgent := firstNonEmpty(
		fields["user_agent"],
		fields["header_user_agent"],
		fields["http_user_agent"],
	)
	caller := fields["caller"]
	if isSourceLocationCaller(caller) {
		caller = ""
	}
	if caller == "" {
		caller = firstNonEmpty(userAgent, fields["org_id"])
	}
	dashboardUID := firstNonEmpty(
		fields["header_x_dashboard_uid"],
		fields["x_dashboard_uid"],
		fields["dashboard_uid"],
	)
	httpPath := firstNonEmpty(
		fields["path"],
		fields["http_path"],
	)

	// Mimir query-frontend logs API user queries (Mimir Ruler bypasses query-frontend).
	origin := metrics.OriginAPIUser
	if isSelfTrimtabCaller(caller, userAgent) || matcher.isExcluded(caller, userAgent, dashboardUID, httpPath, query) {
		origin = metrics.OriginExcludedExplorer
	}
	recordEntry(query, ts, origin, "", "", caller, dashboardUID, httpPath, "loki:mimir-query-frontend", cache, res)
}

// parseLogfmtFields extracts key=value pairs from logfmt lines (supporting both quoted
// strings with backslash escapes and unquoted single-token values). Preserves the first
// non-empty value if a key repeats.
func parseLogfmtFields(line string) map[string]string {
	fields := make(map[string]string)
	n := len(line)
	i := 0
	for i < n {
		for i < n && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
			i++
		}
		if i >= n {
			break
		}
		kStart := i
		for i < n && line[i] != '=' && line[i] != ' ' && line[i] != '\t' && line[i] != '\r' && line[i] != '\n' {
			i++
		}
		if i >= n || line[i] != '=' {
			// Bare key without '=', advance past token if needed.
			continue
		}
		key := line[kStart:i]
		i++ // skip '='
		if i >= n || line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n' {
			continue
		}
		var val string
		if line[i] == '"' {
			qStart := i
			i++
			escaped := false
			closed := false
			for i < n {
				c := line[i]
				if escaped {
					escaped = false
					i++
					continue
				}
				if c == '\\' {
					escaped = true
					i++
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				i++
			}
			if closed {
				if unq, err := strconv.Unquote(line[qStart:i]); err == nil {
					val = unq
				} else {
					val = line[qStart+1 : i-1]
				}
			} else {
				val = line[qStart+1:]
			}
		} else {
			vStart := i
			for i < n && line[i] != ' ' && line[i] != '\t' && line[i] != '\r' && line[i] != '\n' {
				i++
			}
			val = line[vStart:i]
		}
		if key != "" && val != "" {
			if _, exists := fields[key]; !exists {
				fields[key] = val
			}
		}
	}
	return fields
}

func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}
