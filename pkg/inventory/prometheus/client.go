package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/httpclient"
	"trimtab.io/trimtab/pkg/metrics"
)

// MetricMetadata holds Prometheus /api/v1/metadata entries.
type MetricMetadata struct {
	Type string `json:"type"`
	Help string `json:"help"`
}

// InventorySnapshot is the normalized output of either Prometheus HTTP API or an offline JSON fixture.
type InventorySnapshot struct {
	MetricNames        []string                       `json:"metricNames"`
	Metadata           map[string]MetricMetadata      `json:"metadata"`
	SeriesCounts       map[string]int                 `json:"seriesCounts"`
	MetricLabels       map[string][]string            `json:"metricLabels,omitempty"`
	SampleLabelSets    map[string][]map[string]string `json:"sampleLabelSets,omitempty"`
	CollapseOverrides  map[string]map[string]int      `json:"collapseOverrides,omitempty"`
	NativeHistograms   map[string]bool                `json:"nativeHistograms,omitempty"`
	FirstSeenHints     map[string]time.Time           `json:"firstSeenHints,omitempty"`
	LocalTSDBRetention config.Duration                `json:"localTSDBRetention,omitempty"`
	NamingMode         metrics.NamingMode             `json:"namingMode,omitempty"`
	OTLPNameMap        map[string]string              `json:"otlpNameMap,omitempty"`
	Warnings           []string                       `json:"warnings,omitempty"`
}

// CollapseEvaluator executes honest `without`-based label collapse estimations (Invariant #9).
type CollapseEvaluator interface {
	EstimateCollapse(ctx context.Context, metricName string, unusedLabels []string, currentSeries int) (combined *metrics.LabelCollapseEstimate, singles []metrics.LabelCollapseEstimate, err error)
}

// Load loads inventory from either offline FixturePath or live PrometheusURL.
func Load(ctx context.Context, cfg config.InventoryConfig, window time.Duration, now time.Time) (*InventorySnapshot, CollapseEvaluator, error) {
	if cfg.FixturePath != "" {
		return LoadFixture(cfg.FixturePath, cfg)
	}
	if cfg.PrometheusURL != "" {
		return FetchHTTP(ctx, cfg, window, now)
	}
	return nil, nil, fmt.Errorf("inventory requires either fixturePath or prometheusURL")
}

// LoadFixture reads an offline InventorySnapshot JSON file and returns a deterministic FixtureCollapseEvaluator.
func LoadFixture(path string, cfg config.InventoryConfig) (*InventorySnapshot, CollapseEvaluator, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read inventory fixture %s: %w", path, err)
	}
	snap := &InventorySnapshot{
		Metadata:          make(map[string]MetricMetadata),
		SeriesCounts:      make(map[string]int),
		MetricLabels:      make(map[string][]string),
		SampleLabelSets:   make(map[string][]map[string]string),
		CollapseOverrides: make(map[string]map[string]int),
		NativeHistograms:  make(map[string]bool),
		FirstSeenHints:    make(map[string]time.Time),
	}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, nil, fmt.Errorf("parse inventory fixture %s: %w", path, err)
	}
	// Ensure every metric in SeriesCounts or Metadata is in MetricNames.
	nameSet := make(map[string]struct{}, len(snap.MetricNames))
	for _, n := range snap.MetricNames {
		nameSet[n] = struct{}{}
	}
	for n := range snap.SeriesCounts {
		if _, ok := nameSet[n]; !ok {
			nameSet[n] = struct{}{}
			snap.MetricNames = append(snap.MetricNames, n)
		}
	}
	sort.Strings(snap.MetricNames)

	// Populate MetricLabels from SampleLabelSets if omitted.
	for metricName, sets := range snap.SampleLabelSets {
		if len(snap.MetricLabels[metricName]) == 0 {
			lset := make(map[string]struct{})
			for _, ls := range sets {
				for k := range ls {
					lset[k] = struct{}{}
				}
			}
			labels := make([]string, 0, len(lset))
			for k := range lset {
				labels = append(labels, k)
			}
			sort.Strings(labels)
			snap.MetricLabels[metricName] = labels
		}
	}

	if snap.NamingMode == "" {
		snap.NamingMode = metrics.NamingMode(cfg.NamingMode)
	}
	if len(cfg.OTLPNameMap) > 0 {
		if snap.OTLPNameMap == nil {
			snap.OTLPNameMap = make(map[string]string)
		}
		for k, v := range cfg.OTLPNameMap {
			snap.OTLPNameMap[k] = v
		}
	}

	return snap, &FixtureCollapseEvaluator{snap: snap}, nil
}

// FixtureCollapseEvaluator computes exact `without`-grouped series counts over SampleLabelSets
// (or explicit CollapseOverrides), scaling proportionally when SampleLabelSets is a representative sample.
type FixtureCollapseEvaluator struct {
	snap *InventorySnapshot
}

// NewFixtureCollapseEvaluator builds a CollapseEvaluator directly from an in-memory InventorySnapshot.
func NewFixtureCollapseEvaluator(snap *InventorySnapshot) *FixtureCollapseEvaluator {
	return &FixtureCollapseEvaluator{snap: snap}
}

func (f *FixtureCollapseEvaluator) EstimateCollapse(_ context.Context, metricName string, unusedLabels []string, currentSeries int) (*metrics.LabelCollapseEstimate, []metrics.LabelCollapseEstimate, error) {
	if len(unusedLabels) == 0 || currentSeries <= 0 {
		return nil, nil, nil
	}
	sortedUnused := append([]string(nil), unusedLabels...)
	sort.Strings(sortedUnused)

	evalSubset := func(drop []string, isSingle bool) *metrics.LabelCollapseEstimate {
		key := strings.Join(drop, ",")
		q := formatWithoutQuery(metricName, drop)
		if ov, ok := f.snap.CollapseOverrides[metricName]; ok {
			if collapsed, exists := ov[key]; exists {
				return buildCollapseEstimate(drop, currentSeries, collapsed, isSingle, q)
			}
		}
		sets := f.snap.SampleLabelSets[metricName]
		if len(sets) == 0 {
			return nil
		}
		collapsed := simulateWithoutCollapse(sets, drop, currentSeries)
		return buildCollapseEstimate(drop, currentSeries, collapsed, isSingle, q)
	}

	combined := evalSubset(sortedUnused, len(sortedUnused) == 1)
	var singles []metrics.LabelCollapseEstimate
	for _, lbl := range sortedUnused {
		if est := evalSubset([]string{lbl}, true); est != nil {
			singles = append(singles, *est)
		}
	}
	return combined, singles, nil
}

func simulateWithoutCollapse(sets []map[string]string, dropped []string, currentSeries int) int {
	dropSet := make(map[string]struct{}, len(dropped))
	for _, d := range dropped {
		dropSet[d] = struct{}{}
	}
	uniqueGroups := make(map[string]struct{}, len(sets))
	for _, s := range sets {
		keys := make([]string, 0, len(s))
		for k := range s {
			if _, skip := dropSet[k]; skip || k == "__name__" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(s[k])
			b.WriteByte('\x00')
		}
		uniqueGroups[b.String()] = struct{}{}
	}
	if len(sets) == currentSeries {
		return len(uniqueGroups)
	}
	ratio := float64(len(uniqueGroups)) / float64(len(sets))
	collapsed := int(math.Round(ratio * float64(currentSeries)))
	if collapsed < 1 && currentSeries > 0 {
		collapsed = 1
	}
	if collapsed > currentSeries {
		collapsed = currentSeries
	}
	return collapsed
}

func formatWithoutQuery(metricName string, dropped []string) string {
	return fmt.Sprintf("count(count without (%s) (%s))", strings.Join(dropped, ", "), metricName)
}

func buildCollapseEstimate(dropped []string, current, collapsed int, isSingle bool, query string) *metrics.LabelCollapseEstimate {
	savings := 0.0
	if current > 0 {
		savings = float64(current-collapsed) / float64(current) * 100.0
		if savings < 0 {
			savings = 0
		}
	}
	return &metrics.LabelCollapseEstimate{
		DroppedLabels:   dropped,
		CurrentSeries:   current,
		CollapsedSeries: collapsed,
		SavingsPercent:  math.Round(savings*10) / 10,
		IsSingleLabel:   isSingle,
		Query:           query,
	}
}

// HTTPClient implements Prometheus / Mimir inventory fetching and bounded `without` collapse queries.
type HTTPClient struct {
	hc        *httpclient.Client
	selector  string
	tsdbLimit int
}

// NewHTTPClient builds a Prometheus/Mimir HTTPClient backed by pkg/httpclient.
func NewHTTPClient(cfg config.InventoryConfig) (*HTTPClient, error) {
	auth, err := cfg.EffectiveAuth()
	if err != nil {
		return nil, err
	}
	hc, err := httpclient.New(httpclient.Options{
		BaseURL:     cfg.PrometheusURL,
		Timeout:     cfg.Timeout.Duration(),
		TenantID:    cfg.TenantID,
		Headers:     cfg.Headers,
		HeadersEnv:  cfg.HeadersEnv,
		HeadersFile: cfg.HeadersFile,
		Auth:        auth,
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus client: %w", err)
	}
	tsdbLimit := cfg.TSDBLimit
	if tsdbLimit <= 0 {
		tsdbLimit = 10000
	}
	return &HTTPClient{
		hc:        hc,
		selector:  normalizeSelector(cfg.Selector),
		tsdbLimit: tsdbLimit,
	}, nil
}

func (c *HTTPClient) get(ctx context.Context, endpoint string) ([]byte, error) {
	body, err := c.hc.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("prometheus %w", err)
	}
	return body, nil
}

func normalizeSelector(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	return strings.TrimSpace(s)
}

func formatMetricSelector(metricName, selector string) string {
	if selector == "" {
		return metricName
	}
	return fmt.Sprintf("%s{%s}", metricName, selector)
}

// FetchHTTP collects metric family metadata, historical metric names over `window`,
// TSDB head series counts, and checks local TSDB retention (warning only, never hard-failing).
func FetchHTTP(ctx context.Context, cfg config.InventoryConfig, window time.Duration, now time.Time) (*InventorySnapshot, CollapseEvaluator, error) {
	client, err := NewHTTPClient(cfg)
	if err != nil {
		return nil, nil, err
	}
	snap := &InventorySnapshot{
		Metadata:         make(map[string]MetricMetadata),
		SeriesCounts:     make(map[string]int),
		MetricLabels:     make(map[string][]string),
		NativeHistograms: make(map[string]bool),
		FirstSeenHints:   make(map[string]time.Time),
		NamingMode:       metrics.NamingMode(cfg.NamingMode),
		OTLPNameMap:      cfg.OTLPNameMap,
		Warnings:         client.hc.Warnings(),
	}

	// 1. Historical metric names over observation window (Invariant #5: decoupled from ~2h head block)
	start := now.Add(-window)
	q := url.Values{}
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(now.Unix(), 10))
	if client.selector != "" {
		q.Add("match[]", "{"+client.selector+"}")
	}
	namesBody, err := client.get(ctx, "/api/v1/label/__name__/values?"+q.Encode())
	if err != nil {
		return nil, nil, err
	}
	var namesResp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(namesBody, &namesResp); err != nil {
		return nil, nil, fmt.Errorf("decode __name__ values: %w", err)
	}
	nameSet := make(map[string]struct{}, len(namesResp.Data))
	for _, n := range namesResp.Data {
		nameSet[n] = struct{}{}
	}

	// 2. Metric family metadata (/api/v1/metadata)
	metaBody, err := client.get(ctx, "/api/v1/metadata")
	if err != nil {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("prometheus /api/v1/metadata unavailable (falling back to naming heuristics): %v", err))
	} else {
		var metaResp struct {
			Status string                      `json:"status"`
			Data   map[string][]MetricMetadata `json:"data"`
		}
		if err := json.Unmarshal(metaBody, &metaResp); err == nil {
			for k, entries := range metaResp.Data {
				if len(entries) > 0 {
					snap.Metadata[k] = entries[0]
				}
			}
		}
	}

	// 3. TSDB head series counts (/api/v1/status/tsdb?limit=...) or batched `count by (__name__)`
	tsdbLoaded := false
	if client.selector == "" {
		tsdbBody, err := client.get(ctx, fmt.Sprintf("/api/v1/status/tsdb?limit=%d", client.tsdbLimit))
		if err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("prometheus /api/v1/status/tsdb unavailable: %v", err))
		} else {
			var tsdbResp struct {
				Status string `json:"status"`
				Data   struct {
					SeriesCountByMetricName []struct {
						Name  string `json:"name"`
						Value int    `json:"value"`
					} `json:"seriesCountByMetricName"`
				} `json:"data"`
			}
			if err := json.Unmarshal(tsdbBody, &tsdbResp); err == nil && len(tsdbResp.Data.SeriesCountByMetricName) > 0 {
				for _, item := range tsdbResp.Data.SeriesCountByMetricName {
					snap.SeriesCounts[item.Name] = item.Value
					nameSet[item.Name] = struct{}{}
				}
				tsdbLoaded = true
			}
		}
	}

	if !tsdbLoaded && len(nameSet) > 0 {
		if err := client.populateSeriesCountsByQuery(ctx, nameSet, snap.SeriesCounts); err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("prometheus batched series count fallback failed: %v", err))
		}
	}

	// 4. Local TSDB retention check via /api/v1/status/runtimeinfo (Invariant #1: warn only, NEVER hard-fail)
	if rtBody, err := client.get(ctx, "/api/v1/status/runtimeinfo"); err == nil {
		var rtResp struct {
			Status string `json:"status"`
			Data   struct {
				StorageRetention string `json:"storageRetention"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rtBody, &rtResp); err == nil && rtResp.Data.StorageRetention != "" {
			if d, err := config.ParseDuration(strings.Split(rtResp.Data.StorageRetention, " ")[0]); err == nil && d > 0 {
				snap.LocalTSDBRetention = d
				if d.Duration() < window {
					snap.Warnings = append(snap.Warnings, fmt.Sprintf(
						"local Prometheus TSDB retention (%s) is shorter than minObservationWindow (%s); relying on State Ledger + query logs",
						d.String(), config.Duration(window).String(),
					))
				}
			}
		}
	}

	// 4b. Optional early-window probe for FirstSeenHints (seeds Invariant #5 observation age).
	if cfg.ProbeFirstSeen {
		pq := url.Values{}
		pq.Set("start", strconv.FormatInt(start.Unix(), 10))
		pq.Set("end", strconv.FormatInt(start.Add(24*time.Hour).Unix(), 10))
		if client.selector != "" {
			pq.Add("match[]", "{"+client.selector+"}")
		}
		if probeBody, err := client.get(ctx, "/api/v1/label/__name__/values?"+pq.Encode()); err == nil {
			var probeResp struct {
				Status string   `json:"status"`
				Data   []string `json:"data"`
			}
			if err := json.Unmarshal(probeBody, &probeResp); err == nil {
				for _, n := range probeResp.Data {
					snap.FirstSeenHints[n] = start
				}
			}
		}
	}

	// 5. Fetch label names for top-cardinality metrics (bounded by MaxCollapseFamilies)
	snap.MetricNames = make([]string, 0, len(nameSet))
	for n := range nameSet {
		snap.MetricNames = append(snap.MetricNames, n)
	}
	sort.Strings(snap.MetricNames)

	if cfg.CollapseQueriesEnabled() {
		topMetrics := topMetricsBySeries(snap.SeriesCounts, cfg.MaxCollapseFamilies)
		for _, m := range topMetrics {
			lblQ := url.Values{}
			if client.selector != "" {
				lblQ.Add("match[]", fmt.Sprintf(`{__name__=%q,%s}`, m, client.selector))
			} else {
				lblQ.Add("match[]", m)
			}
			if body, err := client.get(ctx, "/api/v1/labels?"+lblQ.Encode()); err == nil {
				var lResp struct {
					Status string   `json:"status"`
					Data   []string `json:"data"`
				}
				if err := json.Unmarshal(body, &lResp); err == nil {
					snap.MetricLabels[m] = lResp.Data
				}
			}
		}
	}

	return snap, client, nil
}

func (c *HTTPClient) populateSeriesCountsByQuery(ctx context.Context, nameSet map[string]struct{}, dst map[string]int) error {
	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)

	const batchSize = 50
	for i := 0; i < len(names); i += batchSize {
		end := i + batchSize
		if end > len(names) {
			end = len(names)
		}
		escaped := make([]string, 0, end-i)
		for _, n := range names[i:end] {
			escaped = append(escaped, regexp.QuoteMeta(n))
		}
		nameMatcher := fmt.Sprintf(`__name__=~"%s"`, strings.Join(escaped, "|"))
		selector := nameMatcher
		if c.selector != "" {
			selector = c.selector + "," + nameMatcher
		}
		expr := fmt.Sprintf(`count by (__name__) ({%s})`, selector)

		q := url.Values{}
		q.Set("query", expr)
		body, err := c.get(ctx, "/api/v1/query?"+q.Encode())
		if err != nil {
			return err
		}
		var resp struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Value  [2]any            `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return err
		}
		for _, item := range resp.Data.Result {
			name := item.Metric["__name__"]
			if name == "" {
				continue
			}
			if valStr, ok := item.Value[1].(string); ok {
				if f, err := strconv.ParseFloat(valStr, 64); err == nil {
					dst[name] = int(math.Round(f))
				}
			}
		}
	}
	return nil
}

func topMetricsBySeries(counts map[string]int, limit int) []string {
	type kv struct {
		name  string
		count int
	}
	list := make([]kv, 0, len(counts))
	for k, v := range counts {
		if v > 0 {
			list = append(list, kv{k, v})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].name < list[j].name
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = item.name
	}
	return out
}

// EstimateCollapse runs exact `count(m)` and `count(count without (...) (m))` queries against Prometheus.
func (c *HTTPClient) EstimateCollapse(ctx context.Context, metricName string, unusedLabels []string, currentSeries int) (*metrics.LabelCollapseEstimate, []metrics.LabelCollapseEstimate, error) {
	if len(unusedLabels) == 0 {
		return nil, nil, nil
	}
	sortedUnused := append([]string(nil), unusedLabels...)
	sort.Strings(sortedUnused)

	targetExpr := formatMetricSelector(metricName, c.selector)

	// Query exact current series count if unknown
	if currentSeries <= 0 {
		n, err := c.instantScalar(ctx, fmt.Sprintf("count(%s)", targetExpr))
		if err != nil {
			return nil, nil, err
		}
		currentSeries = n
	}
	if currentSeries <= 0 {
		return nil, nil, nil
	}

	combinedQuery := formatWithoutQuery(targetExpr, sortedUnused)
	combinedCount, err := c.instantScalar(ctx, combinedQuery)
	if err != nil {
		return nil, nil, err
	}
	combined := buildCollapseEstimate(sortedUnused, currentSeries, combinedCount, len(sortedUnused) == 1, combinedQuery)

	var singles []metrics.LabelCollapseEstimate
	for _, lbl := range sortedUnused {
		q := formatWithoutQuery(targetExpr, []string{lbl})
		cnt, err := c.instantScalar(ctx, q)
		if err != nil {
			continue
		}
		singles = append(singles, *buildCollapseEstimate([]string{lbl}, currentSeries, cnt, true, q))
	}
	return combined, singles, nil
}

func (c *HTTPClient) instantScalar(ctx context.Context, promqlExpr string) (int, error) {
	q := url.Values{}
	q.Set("query", promqlExpr)
	body, err := c.get(ctx, "/api/v1/query?"+q.Encode())
	if err != nil {
		return 0, err
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	if len(resp.Data.Result) == 0 {
		return 0, nil
	}
	valStr, ok := resp.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected prometheus vector value %#v", resp.Data.Result[0].Value[1])
	}
	f, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, err
	}
	return int(math.Round(f)), nil
}
