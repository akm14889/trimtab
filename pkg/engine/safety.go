package engine

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
)

// SourceCounts captures the current run's object counts across configured sources for
// the Baseline Drift Circuit Breaker (Invariant #4).
type SourceCounts struct {
	Dashboards      *int
	GrafanaAlerts   *int
	PromRules       *int
	QueryLogEntries *int
}

// CheckBaselineDrift compares current source counts against persisted ledger baselines.
// If any configured source dropped more than threshold% and acceptShift is false,
// synthesis MUST abort (Invariant #4).
func CheckBaselineDrift(prev state.BaselineCounts, curr SourceCounts, thresholdPct float64, acceptShift bool) (drifted bool, details []string) {
	check := func(name string, prevVal, currVal *int) {
		if prevVal == nil || currVal == nil || *prevVal <= 0 {
			return
		}
		if *currVal >= *prevVal {
			return
		}
		dropPct := float64(*prevVal-*currVal) / float64(*prevVal) * 100.0
		if dropPct > thresholdPct {
			details = append(details, fmt.Sprintf(
				"%s count dropped %.1f%% (%d -> %d, threshold %.1f%%)",
				name, dropPct, *prevVal, *currVal, thresholdPct,
			))
		}
	}

	check("dashboards", prev.Dashboards, curr.Dashboards)
	check("grafana_alerts", prev.GrafanaAlerts, curr.GrafanaAlerts)
	check("prom_rules", prev.PromRules, curr.PromRules)
	check("query_log_entries", prev.QueryLogEntries, curr.QueryLogEntries)

	if len(details) > 0 && !acceptShift {
		return true, details
	}
	return false, details
}

// UpdateBaselineCounts writes current source counts into the ledger (only when safe or explicitly accepted).
func UpdateBaselineCounts(ledger *state.Ledger, curr SourceCounts) {
	if curr.Dashboards != nil {
		v := *curr.Dashboards
		ledger.BaselineCounts.Dashboards = &v
	}
	if curr.GrafanaAlerts != nil {
		v := *curr.GrafanaAlerts
		ledger.BaselineCounts.GrafanaAlerts = &v
	}
	if curr.PromRules != nil {
		v := *curr.PromRules
		ledger.BaselineCounts.PromRules = &v
	}
	if curr.QueryLogEntries != nil {
		v := *curr.QueryLogEntries
		ledger.BaselineCounts.QueryLogEntries = &v
	}
}

// NeverDropMatcher checks default meta-monitoring patterns, user neverDrop regexes,
// and regret-pinned families from the State Ledger (Invariants #6 & #10).
type NeverDropMatcher struct {
	regexes []*regexp.Regexp
	pinned  map[string]struct{}
}

func NewNeverDropMatcher(policy config.PolicyConfig, ledger *state.Ledger) (*NeverDropMatcher, error) {
	m := &NeverDropMatcher{
		pinned: make(map[string]struct{}),
	}
	for _, pat := range policy.EffectiveNeverDropPatterns() {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("compile neverDrop pattern %q: %w", pat, err)
		}
		m.regexes = append(m.regexes, re)
	}
	if ledger != nil {
		for _, fam := range ledger.PinnedNeverDrop {
			m.pinned[fam] = struct{}{}
		}
	}
	return m, nil
}

func (m *NeverDropMatcher) Match(familyName string, members []string) (protected bool, reason string) {
	if _, ok := m.pinned[familyName]; ok {
		return true, "pinned to neverDrop (post-drop regret)"
	}
	for _, re := range m.regexes {
		if re.MatchString(familyName) {
			return true, fmt.Sprintf("matched neverDrop allowlist (%s)", re.String())
		}
		for _, mem := range members {
			if re.MatchString(mem) {
				return true, fmt.Sprintf("member %s matched neverDrop allowlist (%s)", mem, re.String())
			}
		}
	}
	return false, ""
}

// IsSourceExcluded checks whether a UsageRef matches any configured ExcludeSource (Invariant #6).
func IsSourceExcluded(ref metrics.UsageRef, excludes []config.ExcludeSource) bool {
	if ref.Origin == metrics.OriginExcludedExplorer {
		return true
	}
	for _, ex := range excludes {
		val := strings.TrimSpace(ex.Value)
		switch ex.Type {
		case "dashboard_uid":
			if ref.Origin == metrics.OriginGrafanaDashboard && strings.TrimPrefix(ref.SourceID, "grafana-dashboard:") == val {
				return true
			}
		case "dashboard_title":
			if ref.Origin == metrics.OriginGrafanaDashboard && strings.EqualFold(ref.SourceName, val) {
				return true
			}
		case "dashboard_folder":
			if ref.Origin == metrics.OriginGrafanaDashboard && strings.EqualFold(ref.SourceFolder, val) {
				return true
			}
		case "dashboard_tag":
			if ref.Origin == metrics.OriginGrafanaDashboard {
				for _, tag := range ref.SourceTags {
					if strings.EqualFold(tag, val) {
						return true
					}
				}
			}
		case "rule_file":
			if (ref.Origin == metrics.OriginPromAlert || ref.Origin == metrics.OriginPromRecording) &&
				strings.Contains(ref.SourceFolder, val) {
				return true
			}
		case "query_caller":
			if ref.ClientCaller != "" && strings.Contains(strings.ToLower(ref.ClientCaller), strings.ToLower(val)) {
				return true
			}
		case "query_path":
			if ref.HTTPPath != "" && ref.HTTPPath == val {
				return true
			}
		case "query_regex":
			if re, err := regexp.Compile(val); err == nil && re.MatchString(ref.Query) {
				return true
			}
		}
	}
	return false
}

// PlanTranches splits eligible droppable families into bounded rollout tranches capped at
// maxDropPercentPerTranche (default 15% of totalActiveSeries), sorted descending by series
// savings and NEVER splitting a metric family across tranches (Invariant #3).
//
// Any single metric family whose series count alone exceeds maxDropPercentPerTranche is
// returned separately in `oversized` so the tranche cap is never silently breached.
func PlanTranches(
	candidates []metrics.Recommendation,
	totalActiveSeries int,
	maxDropPctPerTranche float64,
) (tranches []metrics.Tranche, oversized []metrics.Recommendation) {
	if len(candidates) == 0 || totalActiveSeries <= 0 {
		return nil, nil
	}

	sorted := make([]metrics.Recommendation, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Series != sorted[j].Series {
			return sorted[i].Series > sorted[j].Series
		}
		return sorted[i].FamilyName < sorted[j].FamilyName
	})

	maxSeriesPerTranche := int(math.Floor(float64(totalActiveSeries) * maxDropPctPerTranche / 100.0))
	if maxSeriesPerTranche < 1 {
		maxSeriesPerTranche = 1
	}

	// Greedy First-Fit Decreasing (largest savings first), never splitting a metric family.
	for _, rec := range sorted {
		if rec.Series > maxSeriesPerTranche {
			rec.AdvisoryOnly = true
			rec.AdvisoryNote = fmt.Sprintf(
				"single family accounts for %.1f%% of active series (> %.1f%% tranche cap); raise maxDropPercentPerTranche or drop explicitly",
				float64(rec.Series)/float64(totalActiveSeries)*100.0, maxDropPctPerTranche,
			)
			oversized = append(oversized, rec)
			continue
		}

		placed := false
		for i := range tranches {
			if tranches[i].Series+rec.Series <= maxSeriesPerTranche {
				rec.TrancheNumber = tranches[i].Number
				tranches[i].Items = append(tranches[i].Items, rec)
				tranches[i].Series += rec.Series
				tranches[i].PercentEstate = math.Round(float64(tranches[i].Series)/float64(totalActiveSeries)*1000) / 10
				placed = true
				break
			}
		}
		if !placed {
			num := len(tranches) + 1
			rec.TrancheNumber = num
			tranches = append(tranches, metrics.Tranche{
				Number:        num,
				Series:        rec.Series,
				PercentEstate: math.Round(float64(rec.Series)/float64(totalActiveSeries)*1000) / 10,
				Items:         []metrics.Recommendation{rec},
			})
		}
	}
	return tranches, oversized
}
