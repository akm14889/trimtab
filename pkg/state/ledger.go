package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"trimtab.io/trimtab/pkg/metrics"
)

const CurrentSchemaVersion = 1

// BaselineCounts stores per-source object counts for the Baseline Drift Circuit Breaker (Invariant #4).
type BaselineCounts struct {
	Dashboards        *int     `json:"dashboards,omitempty"`
	GrafanaAlerts     *int     `json:"grafana_alerts,omitempty"`
	PromRules         *int     `json:"prom_rules,omitempty"`
	RulerRules        *int     `json:"ruler_rules,omitempty"`
	KEDATriggers      *int     `json:"keda_triggers,omitempty"`
	PromAdapterRules  *int     `json:"prom_adapter_rules,omitempty"`
	RolloutTemplates     *int     `json:"rollout_templates,omitempty"`
	FlaggerTemplates     *int     `json:"flagger_templates,omitempty"`
	AlertmanagerMatchers *int     `json:"alertmanager_matchers,omitempty"`
	PersesDashboards     *int     `json:"perses_dashboards,omitempty"`
	SLOs                 *int     `json:"slos,omitempty"`
	QueryLogEntries      *int     `json:"query_log_entries,omitempty"`
	QueryLogDailyRate *float64 `json:"query_log_daily_rate,omitempty"`
}

// QueryLogCoverage tracks cold-start query log observation coverage (Invariant #1).
type QueryLogCoverage struct {
	EarliestTimestamp time.Time      `json:"earliest_timestamp,omitempty"`
	LatestTimestamp   time.Time      `json:"latest_timestamp,omitempty"`
	CoveredDays       float64        `json:"covered_days"`
	CoveredDates      map[string]int `json:"covered_dates,omitempty"`
	APIUserQueries    int            `json:"api_user_queries"`
	RuleEvalQueries   int            `json:"rule_eval_queries"`
	ExcludedQueries   int            `json:"excluded_queries"`
}

// SeenFamily persists family discovery timestamps decoupled from TSDB head block (Invariant #5).
type SeenFamily struct {
	FirstSeenAt         time.Time `json:"first_seen_at"`
	LastSeenAt          time.Time `json:"last_seen_at"`
	LastQueriedAt       time.Time `json:"last_queried_at,omitempty"`
	LastBucketQueriedAt time.Time `json:"last_bucket_queried_at,omitempty"`
}

// TrancheRecord tracks rollout state and soak window per tranche (Invariant #3).
type TrancheRecord struct {
	Number            int       `json:"number"`
	StartedAt         time.Time `json:"started_at"`
	ActivatedAt       time.Time `json:"activated_at,omitempty"`
	ConfirmedAt       time.Time `json:"confirmed_at,omitempty"`
	SoakUntil         time.Time `json:"soak_until,omitempty"`
	Status            string    `json:"status"` // "pending_confirmation", "soaking", "soaked", "blocked_regret" (legacy: "active")
	Families          []string  `json:"families"`
	Series            int       `json:"series"`
	UnresolvedRegrets int       `json:"unresolved_regrets"`
}

// ActiveDropRecord tracks an actively dropped metric family for Regret Detection (Invariant #10).
type ActiveDropRecord struct {
	FamilyName   string             `json:"family_name"`
	Action       metrics.ActionType `json:"action"`
	DroppedAt    time.Time          `json:"dropped_at"`
	Tranche      int                `json:"tranche"`
	SeriesAtDrop int                `json:"series_at_drop"`
	Reason       string             `json:"reason,omitempty"`
	MetricType   metrics.MetricType `json:"metric_type,omitempty"`
	Members      []string           `json:"members,omitempty"`
	NamingMode   metrics.NamingMode `json:"naming_mode,omitempty"`
	OTLPName     string             `json:"otlp_name,omitempty"`
}

// Ledger is the persistent .trimtab-state.json State Ledger.
type Ledger struct {
	Version          int                         `json:"version"`
	UpdatedAt        time.Time                   `json:"updated_at"`
	BaselineCounts   BaselineCounts              `json:"baseline_counts"`
	QueryLogCoverage QueryLogCoverage            `json:"query_log_coverage"`
	SeenFamilies     map[string]*SeenFamily      `json:"seen_families"`
	Tranches         []TrancheRecord             `json:"tranches,omitempty"`
	ActiveDrops      map[string]*ActiveDropRecord `json:"active_drops"`
	PinnedNeverDrop  []string                    `json:"pinned_never_drop,omitempty"`
	RegretHistory    []metrics.RegretEvent       `json:"regret_history,omitempty"`
}

// NewLedger creates an initialized empty State Ledger.
func NewLedger() *Ledger {
	return &Ledger{
		Version:      CurrentSchemaVersion,
		SeenFamilies: make(map[string]*SeenFamily),
		ActiveDrops:  make(map[string]*ActiveDropRecord),
		QueryLogCoverage: QueryLogCoverage{
			CoveredDates: make(map[string]int),
		},
	}
}

// Load reads a Ledger from disk, returning a fresh empty Ledger if the file does not exist.
func Load(path string) (*Ledger, error) {
	if path == "" {
		return NewLedger(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewLedger(), nil
		}
		return nil, fmt.Errorf("read state ledger %s: %w", path, err)
	}
	l := NewLedger()
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("parse state ledger %s: %w", path, err)
	}
	if l.Version == 0 {
		l.Version = CurrentSchemaVersion
	}
	if l.SeenFamilies == nil {
		l.SeenFamilies = make(map[string]*SeenFamily)
	}
	if l.ActiveDrops == nil {
		l.ActiveDrops = make(map[string]*ActiveDropRecord)
	}
	if l.QueryLogCoverage.CoveredDates == nil {
		l.QueryLogCoverage.CoveredDates = make(map[string]int)
	}
	return l, nil
}

// Save atomically persists the Ledger to disk (temp file + rename).
func (l *Ledger) Save(path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	sort.Strings(l.PinnedNeverDrop)
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state ledger: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".trimtab-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("commit state ledger %s: %w", path, err)
	}
	return nil
}

// RecordSeenFamily updates first_seen_at (keeping the earliest known timestamp) and last_seen_at.
func (l *Ledger) RecordSeenFamily(family string, observedFirst, observedLast time.Time) *SeenFamily {
	sf, ok := l.SeenFamilies[family]
	if !ok {
		sf = &SeenFamily{
			FirstSeenAt: observedFirst,
			LastSeenAt:  observedLast,
		}
		l.SeenFamilies[family] = sf
		return sf
	}
	if !observedFirst.IsZero() && (sf.FirstSeenAt.IsZero() || observedFirst.Before(sf.FirstSeenAt)) {
		sf.FirstSeenAt = observedFirst
	}
	if observedLast.After(sf.LastSeenAt) {
		sf.LastSeenAt = observedLast
	}
	return sf
}

// RecordLastQueried updates last_queried_at for a metric family.
func (l *Ledger) RecordLastQueried(family string, ts time.Time) {
	if ts.IsZero() {
		return
	}
	sf, ok := l.SeenFamilies[family]
	if !ok {
		sf = &SeenFamily{FirstSeenAt: ts, LastSeenAt: ts}
		l.SeenFamilies[family] = sf
	}
	if ts.After(sf.LastQueriedAt) {
		sf.LastQueriedAt = ts
	}
}

// RecordLastBucketQueried updates last_bucket_queried_at for a histogram family.
func (l *Ledger) RecordLastBucketQueried(family string, ts time.Time) {
	if ts.IsZero() {
		return
	}
	sf, ok := l.SeenFamilies[family]
	if !ok {
		sf = &SeenFamily{FirstSeenAt: ts, LastSeenAt: ts}
		l.SeenFamilies[family] = sf
	}
	if ts.After(sf.LastBucketQueriedAt) {
		sf.LastBucketQueriedAt = ts
	}
}

// UpdateQueryLogCoverage updates trailing-window UTC calendar date coverage and query-log totals.
func (l *Ledger) UpdateQueryLogCoverage(
	observedDates map[string]int,
	earliest, latest time.Time,
	apiUser, ruleEval, excluded int,
	now time.Time,
	window time.Duration,
) {
	if l.QueryLogCoverage.CoveredDates == nil {
		l.QueryLogCoverage.CoveredDates = make(map[string]int)
	}
	for date, cnt := range observedDates {
		if cnt <= 0 || date == "" {
			continue
		}
		if cnt > l.QueryLogCoverage.CoveredDates[date] {
			l.QueryLogCoverage.CoveredDates[date] = cnt
		}
	}
	if !earliest.IsZero() {
		if l.QueryLogCoverage.EarliestTimestamp.IsZero() || earliest.Before(l.QueryLogCoverage.EarliestTimestamp) {
			l.QueryLogCoverage.EarliestTimestamp = earliest
		}
	}
	if !latest.IsZero() {
		if l.QueryLogCoverage.LatestTimestamp.IsZero() || latest.After(l.QueryLogCoverage.LatestTimestamp) {
			l.QueryLogCoverage.LatestTimestamp = latest
		}
	}

	windowEnd := now.UTC()
	if windowEnd.IsZero() {
		windowEnd = l.QueryLogCoverage.LatestTimestamp.UTC()
	}

	if len(l.QueryLogCoverage.CoveredDates) > 0 {
		covered := 0
		if window > 0 && !windowEnd.IsZero() {
			startDay := windowEnd.Add(-window).UTC().Format("2006-01-02")
			endDay := windowEnd.UTC().Format("2006-01-02")
			for date, cnt := range l.QueryLogCoverage.CoveredDates {
				if cnt > 0 && date >= startDay && date <= endDay {
					covered++
				}
			}
			maxDays := window.Hours() / 24.0
			if window >= 24*time.Hour && float64(covered) > maxDays {
				l.QueryLogCoverage.CoveredDays = maxDays
			} else {
				l.QueryLogCoverage.CoveredDays = float64(covered)
			}
		} else {
			for _, cnt := range l.QueryLogCoverage.CoveredDates {
				if cnt > 0 {
					covered++
				}
			}
			l.QueryLogCoverage.CoveredDays = float64(covered)
		}
	} else if !l.QueryLogCoverage.EarliestTimestamp.IsZero() && !l.QueryLogCoverage.LatestTimestamp.IsZero() {
		// Backward-compatible fallback when neither ledger nor current run provides CoveredDates.
		effStart := l.QueryLogCoverage.EarliestTimestamp
		effEnd := l.QueryLogCoverage.LatestTimestamp
		if window > 0 && !windowEnd.IsZero() {
			windowStart := windowEnd.Add(-window)
			if effEnd.Before(windowStart) {
				l.QueryLogCoverage.CoveredDays = 0
				effStart = effEnd
			} else {
				if effStart.Before(windowStart) {
					effStart = windowStart
				}
				if effEnd.After(windowEnd) {
					effEnd = windowEnd
				}
			}
		}
		span := effEnd.Sub(effStart)
		if span >= 0 {
			l.QueryLogCoverage.CoveredDays = span.Hours() / 24.0
		} else {
			l.QueryLogCoverage.CoveredDays = 0
		}
	}
	l.QueryLogCoverage.APIUserQueries = apiUser
	l.QueryLogCoverage.RuleEvalQueries = ruleEval
	l.QueryLogCoverage.ExcludedQueries = excluded
}

// PinNeverDrop permanently pins a family to the ledger neverDrop list (e.g. on Regret).
func (l *Ledger) PinNeverDrop(family string) {
	for _, p := range l.PinnedNeverDrop {
		if p == family {
			return
		}
	}
	l.PinnedNeverDrop = append(l.PinnedNeverDrop, family)
	sort.Strings(l.PinnedNeverDrop)
}

func (l *Ledger) IsPinnedNeverDrop(family string) bool {
	for _, p := range l.PinnedNeverDrop {
		if p == family {
			return true
		}
	}
	return false
}

// RecordRegret removes the family from ActiveDrops, pins it to neverDrop,
// marks the latest tranche as blocked_regret, and appends the RegretEvent.
func (l *Ledger) RecordRegret(ev metrics.RegretEvent) {
	delete(l.ActiveDrops, ev.FamilyName)
	l.PinNeverDrop(ev.FamilyName)
	l.RegretHistory = append(l.RegretHistory, ev)
	if len(l.Tranches) > 0 {
		last := &l.Tranches[len(l.Tranches)-1]
		last.UnresolvedRegrets++
		last.Status = "blocked_regret"
	}
}

// LatestTranche returns the most recently activated tranche record, or nil if none.
func (l *Ledger) LatestTranche() *TrancheRecord {
	if len(l.Tranches) == 0 {
		return nil
	}
	return &l.Tranches[len(l.Tranches)-1]
}

// RecordTrancheActivation records newly synthesized drops into ActiveDrops and appends/updates the tranche record.
func (l *Ledger) RecordTrancheActivation(trancheNum int, now time.Time, soak time.Duration, immediate bool, recs []metrics.Recommendation) {
	var families []string
	totalSeries := 0
	for _, r := range recs {
		families = append(families, r.FamilyName)
		totalSeries += r.Series
		if _, exists := l.ActiveDrops[r.FamilyName]; !exists {
			members := append([]string(nil), r.Members...)
			l.ActiveDrops[r.FamilyName] = &ActiveDropRecord{
				FamilyName:   r.FamilyName,
				Action:       r.Action,
				DroppedAt:    now,
				Tranche:      trancheNum,
				SeriesAtDrop: r.Series,
				Reason:       r.Reason,
				MetricType:   r.MetricType,
				Members:      members,
				NamingMode:   r.NamingMode,
				OTLPName:     r.OTLPName,
			}
		}
	}
	sort.Strings(families)

	status := "pending_confirmation"
	var confirmedAt, soakUntil time.Time
	if immediate {
		status = "soaking"
		confirmedAt = now
		soakUntil = now.Add(soak)
	}

	// Avoid duplicating a tranche record if re-running inside the same active tranche.
	if last := l.LatestTranche(); last != nil && last.Number == trancheNum {
		last.Families = families
		last.Series = totalSeries
		return
	}
	l.Tranches = append(l.Tranches, TrancheRecord{
		Number:      trancheNum,
		StartedAt:   now,
		ActivatedAt: now,
		ConfirmedAt: confirmedAt,
		SoakUntil:   soakUntil,
		Status:      status,
		Families:    families,
		Series:      totalSeries,
	})
}

// ConfirmLatestTranche transitions a pending_confirmation tranche into soaking and starts its soak timer.
func (l *Ledger) ConfirmLatestTranche(now time.Time, soak time.Duration) bool {
	last := l.LatestTranche()
	if last == nil || last.Status != "pending_confirmation" {
		return false
	}
	last.Status = "soaking"
	last.ConfirmedAt = now
	last.SoakUntil = now.Add(soak)
	return true
}

func IntPtr(v int) *int         { return &v }
func Float64Ptr(v float64) *float64 { return &v }
