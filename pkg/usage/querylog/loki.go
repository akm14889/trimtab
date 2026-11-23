package querylog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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

var (
	// Mimir query-frontend logfmt query extraction: query="..." or param_query="..."
	logfmtQuotedQueryRe = regexp.MustCompile(`(?:^|\s)(?:param_)?query="((?:[^"\\]|\\.)*)"`)
	// Mimir query-frontend series/label matchers: param_match[]="..." or match[]="..."
	logfmtQuotedMatchRe = regexp.MustCompile(`(?:^|\s)(?:param_)?match\[\]="((?:[^"\\]|\\.)*)"`)
	logfmtCallerRe      = regexp.MustCompile(`(?:^|\s)(?:caller|org_id)="?([^\s"]+)"?`)
	logfmtUserAgentRe   = regexp.MustCompile(`(?:^|\s)user_agent="((?:[^"\\]|\\.)*)"|(?:^|\s)user_agent=([^\s"]+)`)
)

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
	query := ""
	if m := logfmtQuotedQueryRe.FindStringSubmatch(line); len(m) >= 2 {
		if unq, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
			query = unq
		} else {
			query = m[1]
		}
	} else if mm := logfmtQuotedMatchRe.FindStringSubmatch(line); len(mm) >= 2 {
		if unq, err := strconv.Unquote(`"` + mm[1] + `"`); err == nil {
			query = unq
		} else {
			query = mm[1]
		}
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}

	caller := ""
	if cm := logfmtCallerRe.FindStringSubmatch(line); len(cm) > 1 {
		caller = cm[1]
	}
	userAgent := ""
	if uam := logfmtUserAgentRe.FindStringSubmatch(line); len(uam) > 1 {
		if uam[1] != "" {
			if unq, err := strconv.Unquote(`"` + uam[1] + `"`); err == nil {
				userAgent = unq
			} else {
				userAgent = uam[1]
			}
		} else {
			userAgent = uam[2]
		}
	}
	if caller == "" && userAgent != "" {
		caller = userAgent
	}

	// Mimir query-frontend logs API user queries (Mimir Ruler bypasses query-frontend).
	origin := metrics.OriginAPIUser
	if isSelfTrimtabCaller(caller, userAgent) || matcher.isExcluded(caller, "", "", query) {
		origin = metrics.OriginExcludedExplorer
	}
	recordEntry(query, ts, origin, "", "", caller, "", "", "loki:mimir-query-frontend", cache, res)
}
