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
	Dashboards      *int `json:"dashboards,omitempty"`
	GrafanaAlerts   *int `json:"grafana_alerts,omitempty"`
	PromRules       *int `json:"prom_rules,omitempty"`
	QueryLogEntries *int `json:"query_log_entries,omitempty"`
}

// QueryLogCoverage tracks cold-start query log observation coverage (Invariant #1).
type QueryLogCoverage struct {
	EarliestTimestamp time.Time `json:"earliest_timestamp,omitempty"`
	LatestTimestamp   time.Time `json:"latest_timestamp,omitempty"`
	CoveredDays       float64   `json:"covered_days"`
	APIUserQueries    int       `json:"api_user_queries"`
	RuleEvalQueries   int       `json:"rule_eval_queries"`
	ExcludedQueries   int       `json:"excluded_queries"`
}

// SeenFamily persists family discovery timestamps decoupled from TSDB head block (Invariant #5).
type SeenFamily struct {
	FirstSeenAt   time.Time `json:"first_seen_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	LastQueriedAt time.Time `json:"last_queried_at,omitempty"`
}

// TrancheRecord tracks rollout state and soak window per tranche (Invariant #3).
type TrancheRecord struct {
	Number            int       `json:"number"`
	StartedAt         time.Time `json:"started_at"`
	SoakUntil         time.Time `json:"soak_until"`
	Status            string    `json:"status"` // "active", "soaked", "blocked_regret"
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

// UpdateQueryLogCoverage widens the tracked query-log time coverage across runs.
func (l *Ledger) UpdateQueryLogCoverage(earliest, latest time.Time, apiUser, ruleEval, excluded int) {
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
	if !l.QueryLogCoverage.EarliestTimestamp.IsZero() && !l.QueryLogCoverage.LatestTimestamp.IsZero() {
		span := l.QueryLogCoverage.LatestTimestamp.Sub(l.QueryLogCoverage.EarliestTimestamp)
		if span >= 0 {
			l.QueryLogCoverage.CoveredDays = span.Hours() / 24.0
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
func (l *Ledger) RecordTrancheActivation(trancheNum int, now time.Time, soak time.Duration, recs []metrics.Recommendation) {
	var families []string
	totalSeries := 0
	for _, r := range recs {
		families = append(families, r.FamilyName)
		totalSeries += r.Series
		if _, exists := l.ActiveDrops[r.FamilyName]; !exists {
			l.ActiveDrops[r.FamilyName] = &ActiveDropRecord{
				FamilyName:   r.FamilyName,
				Action:       r.Action,
				DroppedAt:    now,
				Tranche:      trancheNum,
				SeriesAtDrop: r.Series,
				Reason:       r.Reason,
			}
		}
	}
	sort.Strings(families)

	// Avoid duplicating a tranche record if re-running inside the same active tranche.
	if last := l.LatestTranche(); last != nil && last.Number == trancheNum {
		last.Families = families
		last.Series = totalSeries
		return
	}
	l.Tranches = append(l.Tranches, TrancheRecord{
		Number:    trancheNum,
		StartedAt: now,
		SoakUntil: now.Add(soak),
		Status:    "active",
		Families:  families,
		Series:    totalSeries,
	})
}

func IntPtr(v int) *int { return &v }
