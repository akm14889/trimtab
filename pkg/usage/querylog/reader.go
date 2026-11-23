package querylog

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promql"
)

// QueryLogEntry represents one classified entry from Prometheus query_log_file,
// prom-analytics-proxy, or Loki-shipped query logs.
type QueryLogEntry struct {
	Query        string                `json:"query"`
	Timestamp    time.Time             `json:"timestamp"`
	Origin       metrics.UsageOrigin   `json:"origin"`
	RuleGroup    string                `json:"ruleGroup,omitempty"`
	RuleFile     string                `json:"ruleFile,omitempty"`
	ClientCaller string                `json:"clientCaller,omitempty"`
	HTTPPath     string                `json:"httpPath,omitempty"`
	SourceFile   string                `json:"sourceFile,omitempty"`
	Accesses     []promql.MetricAccess `json:"accesses,omitempty"`
}

// Result holds all parsed and classified query-log entries plus coverage metrics.
type Result struct {
	Configured        bool
	Entries           []QueryLogEntry
	Refs              []metrics.UsageRef
	EarliestTimestamp time.Time
	LatestTimestamp   time.Time
	TotalEntries      int
	APIUserCount      int
	RuleEvalCount     int
	ExcludedCount     int
	FilesScanned      int
	Warnings          []string
}

type excludeMatcher struct {
	callers map[string]struct{}
	paths   map[string]struct{}
	regexes []*regexp.Regexp
}

func newExcludeMatcher(excludes []config.ExcludeSource) *excludeMatcher {
	m := &excludeMatcher{
		callers: make(map[string]struct{}),
		paths:   make(map[string]struct{}),
	}
	for _, ex := range excludes {
		switch ex.Type {
		case "query_caller":
			m.callers[strings.ToLower(strings.TrimSpace(ex.Value))] = struct{}{}
		case "query_path":
			m.paths[strings.TrimSpace(ex.Value)] = struct{}{}
		case "query_regex":
			if re, err := regexp.Compile(ex.Value); err == nil {
				m.regexes = append(m.regexes, re)
			}
		}
	}
	return m
}

func (m *excludeMatcher) isExcluded(caller, path, query string) bool {
	if caller != "" {
		lc := strings.ToLower(caller)
		if _, ok := m.callers[lc]; ok {
			return true
		}
		for c := range m.callers {
			if strings.Contains(lc, c) {
				return true
			}
		}
	}
	if path != "" {
		if _, ok := m.paths[path]; ok {
			return true
		}
	}
	for _, re := range m.regexes {
		if re.MatchString(query) {
			return true
		}
	}
	return false
}

// ReadSources loads all configured query-log sources (local globs/dirs and Loki).
func ReadSources(ctx context.Context, sources []config.QueryLogSourceConfig, excludes []config.ExcludeSource, window time.Duration, now time.Time) (*Result, error) {
	res := &Result{}
	if len(sources) == 0 {
		return res, nil
	}
	res.Configured = true
	matcher := newExcludeMatcher(excludes)
	parseCache := make(map[string]*promql.ExtractResult)

	for _, src := range sources {
		if src.Loki != nil && src.Loki.URL != "" {
			if err := readLokiSource(ctx, src.Loki, matcher, parseCache, window, now, res); err != nil {
				return nil, err
			}
		}
		if src.Glob != "" {
			files, err := expandQueryLogFiles(src.Glob)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				if err := readQueryLogFile(f, src.Format, matcher, parseCache, res); err != nil {
					return nil, err
				}
			}
		}
	}
	return res, nil
}

func expandQueryLogFiles(pattern string) ([]string, error) {
	info, err := os.Stat(pattern)
	if err == nil && info.IsDir() {
		entries, err := os.ReadDir(pattern)
		if err != nil {
			return nil, fmt.Errorf("read query log dir %s: %w", pattern, err)
		}
		var files []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := strings.ToLower(e.Name())
			if strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json") ||
				strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".gz") {
				files = append(files, filepath.Join(pattern, e.Name()))
			}
		}
		sort.Strings(files)
		return files, nil
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid query log glob %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("query log glob matched no files: %q", pattern)
	}
	sort.Strings(matches)
	return matches, nil
}

func readQueryLogFile(path, format string, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open query log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var r io.Reader = f
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("gunzip query log %s: %w", path, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}

	res.FilesScanned++
	br := bufio.NewReaderSize(r, 256*1024)

	// Peek first non-whitespace byte to detect JSON array (prom-analytics-proxy export) vs JSONL.
	var firstByte byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read query log %s: %w", path, err)
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			firstByte = b
			_ = br.UnreadByte()
			break
		}
	}

	if firstByte == '[' {
		data, err := io.ReadAll(br)
		if err != nil {
			return fmt.Errorf("read JSON array query log %s: %w", path, err)
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(data, &rows); err != nil {
			return fmt.Errorf("parse JSON array query log %s: %w", path, err)
		}
		for _, raw := range rows {
			ingestJSONLine(raw, format, path, matcher, cache, res)
		}
		return nil
	}

	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ingestJSONLine([]byte(line), format, path, matcher, cache, res)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan query log %s: %w", path, err)
	}
	return nil
}

// rawQueryLogLine supports both Prometheus query_log_file JSONL and prom-analytics-proxy export schemas.
type rawQueryLogLine struct {
	// Prometheus query_log_file schema:
	Params *struct {
		Query string `json:"query"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"params"`
	HTTPRequest *struct {
		ClientIP string              `json:"clientIP"`
		Method   string              `json:"method"`
		Path     string              `json:"path"`
		Headers  map[string]any      `json:"headers"`
	} `json:"httpRequest"`
	RuleGroup *struct {
		File string `json:"file"`
		Name string `json:"name"`
	} `json:"ruleGroup"`
	TS string `json:"ts"`

	// prom-analytics-proxy / generic schema:
	Query      string         `json:"query"`
	QueryParam string         `json:"queryParam"`
	Timestamp  string         `json:"timestamp"`
	Time       string         `json:"time"`
	ClientIP   string         `json:"clientIP"`
	Caller     string         `json:"caller"`
	Path       string         `json:"path"`
	Headers    map[string]any `json:"httpHeaders"`
	Origin     string         `json:"origin"`
}

func ingestJSONLine(raw []byte, format, sourceFile string, matcher *excludeMatcher, cache map[string]*promql.ExtractResult, res *Result) {
	var row rawQueryLogLine
	if err := json.Unmarshal(raw, &row); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("querylog %s: skipped malformed JSON line", filepath.Base(sourceFile)))
		return
	}

	query := ""
	if row.Params != nil && row.Params.Query != "" {
		query = row.Params.Query
	} else if row.Query != "" {
		query = row.Query
	} else if row.QueryParam != "" {
		query = row.QueryParam
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}

	ts := parseTimestamp(row.TS, row.Timestamp, row.Time)
	if ts.IsZero() && row.Params != nil {
		ts = parseTimestamp(row.Params.End, row.Params.Start)
	}

	caller := row.Caller
	httpPath := row.Path
	isPromAnalyticsProxy := format == "prom-analytics-proxy" || (row.Params == nil && (row.Query != "" || row.QueryParam != ""))

	if row.HTTPRequest != nil {
		if caller == "" {
			caller = extractCallerFromHeaders(row.HTTPRequest.Headers, row.HTTPRequest.ClientIP)
		}
		if httpPath == "" {
			httpPath = row.HTTPRequest.Path
		}
	} else if isPromAnalyticsProxy {
		if caller == "" {
			caller = extractCallerFromHeaders(row.Headers, row.ClientIP)
		}
	}

	// Origin classification (Invariant #2):
	// rule-eval: ruleGroup != nil || (httpRequest == nil && !isPromAnalyticsProxy)
	var origin metrics.UsageOrigin
	var ruleGroup, ruleFile string
	if row.RuleGroup != nil {
		origin = metrics.OriginRuleEval
		ruleGroup = row.RuleGroup.Name
		ruleFile = row.RuleGroup.File
	} else if row.HTTPRequest == nil && !isPromAnalyticsProxy {
		origin = metrics.OriginRuleEval
	} else if matcher.isExcluded(caller, httpPath, query) {
		origin = metrics.OriginExcludedExplorer
	} else {
		origin = metrics.OriginAPIUser
	}

	recordEntry(query, ts, origin, ruleGroup, ruleFile, caller, httpPath, sourceFile, cache, res)
}

func recordEntry(query string, ts time.Time, origin metrics.UsageOrigin, ruleGroup, ruleFile, caller, httpPath, sourceFile string, cache map[string]*promql.ExtractResult, res *Result) {
	res.TotalEntries++
	if !ts.IsZero() {
		if res.EarliestTimestamp.IsZero() || ts.Before(res.EarliestTimestamp) {
			res.EarliestTimestamp = ts
		}
		if res.LatestTimestamp.IsZero() || ts.After(res.LatestTimestamp) {
			res.LatestTimestamp = ts
		}
	}

	switch origin {
	case metrics.OriginAPIUser:
		res.APIUserCount++
	case metrics.OriginRuleEval:
		res.RuleEvalCount++
	case metrics.OriginExcludedExplorer:
		res.ExcludedCount++
	}

	var accesses []promql.MetricAccess
	if origin != metrics.OriginExcludedExplorer {
		extracted, cached := cache[query]
		if !cached {
			var err error
			extracted, err = promql.Extract(query)
			if err != nil {
				// Cache nil so we don't spam warnings for repeated invalid queries
				cache[query] = nil
			} else {
				cache[query] = extracted
			}
		}
		if extracted != nil {
			accesses = extracted.Accesses
		}
	}

	entry := QueryLogEntry{
		Query:        query,
		Timestamp:    ts,
		Origin:       origin,
		RuleGroup:    ruleGroup,
		RuleFile:     ruleFile,
		ClientCaller: caller,
		HTTPPath:     httpPath,
		SourceFile:   sourceFile,
		Accesses:     accesses,
	}
	res.Entries = append(res.Entries, entry)

	if origin == metrics.OriginExcludedExplorer {
		return
	}

	sourceID := "querylog:" + string(origin)
	sourceName := "query-log (" + string(origin) + ")"
	if caller != "" {
		sourceID = "querylog:caller:" + caller
		sourceName = "query-log caller " + caller
	} else if ruleGroup != "" {
		sourceID = "querylog:rulegroup:" + ruleGroup
		sourceName = "rule-eval " + ruleGroup
	}

	for _, acc := range accesses {
		res.Refs = append(res.Refs, metrics.UsageRef{
			MetricName:     acc.MetricName,
			MetricRegex:    acc.MetricRegex,
			IsRegex:        acc.IsRegex,
			Origin:         origin,
			SourceID:       sourceID,
			SourceName:     sourceName,
			Query:          query,
			Timestamp:      ts,
			ExplicitLabels: acc.ExplicitLabels,
			PreservesAll:   acc.PreservesAll,
			WithoutLabels:  acc.WithoutLabels,
			ClientCaller:   caller,
			HTTPPath:       httpPath,
		})
	}
}

func extractCallerFromHeaders(headers map[string]any, clientIP string) string {
	for _, key := range []string{"X-Dashboard-UID", "X-Grafana-Org-Id", "X-Panel-Id", "User-Agent", "user-agent"} {
		if v, ok := headers[key]; ok {
			if s := headerString(v); s != "" {
				return s
			}
		}
	}
	return clientIP
}

func headerString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

func parseTimestamp(candidates ...string) time.Time {
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, c); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
