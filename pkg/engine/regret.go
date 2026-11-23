package engine

import (
	"fmt"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/usage/querylog"
)

// DetectRegrets scans `api-user` query-log entries for queries hitting any metric family
// currently recorded in `ledger.ActiveDrops` after its `DroppedAt` timestamp (Invariant #10).
//
// Default threshold = 1 (a single on-call SRE hitting an empty graph fires regret).
// When regret fires: emits RegretEvent, removes metric from ActiveDrops, pins it to
// ledger.PinnedNeverDrop, and blocks advancing to the next rollout tranche.
func DetectRegrets(
	cat *FamilyCatalog,
	ledger *state.Ledger,
	ql *querylog.Result,
	policy config.PolicyConfig,
	now time.Time,
) []metrics.RegretEvent {
	if ledger == nil || len(ledger.ActiveDrops) == 0 || ql == nil || len(ql.Entries) == 0 {
		return nil
	}
	threshold := policy.EffectiveRegretThreshold()

	excludedCallers := make(map[string]struct{}, len(policy.ExcludeRegretCallers))
	for _, c := range policy.ExcludeRegretCallers {
		excludedCallers[strings.ToLower(strings.TrimSpace(c))] = struct{}{}
	}
	isRegretCallerExcluded := func(caller string) bool {
		if caller == "" || len(excludedCallers) == 0 {
			return false
		}
		lc := strings.ToLower(caller)
		for ex := range excludedCallers {
			if lc == ex || strings.Contains(lc, ex) {
				return true
			}
		}
		return false
	}

	type hitTracker struct {
		count      int
		latestTS   time.Time
		sampleExpr string
	}
	hits := make(map[string]*hitTracker)

	for _, entry := range ql.Entries {
		// Only live API/user queries trigger regret (rule-evals and excluded explorers do not).
		if entry.Origin != metrics.OriginAPIUser {
			continue
		}
		if isRegretCallerExcluded(entry.ClientCaller) {
			continue
		}

		matchedInEntry := make(map[string]struct{})
		for _, acc := range entry.Accesses {
			var matches []FamilyMatch
			if acc.IsRegex {
				// Ignore pure catch-all wildcard queries (e.g. `{__name__=~".+"}`) if any slipped past exclusions?
				// Wait: only exact or targeted regex matches should blame a specific dropped family,
				// OR any non-excluded api-user query that specifically matches an active drop.
				if acc.MetricRegex == ".+" || acc.MetricRegex == ".*" {
					continue
				}
				matches = cat.MatchRegex(acc.MetricRegex)
			} else {
				matches = cat.MatchExactName(acc.MetricName)
				// Also support active drops that may have disappeared from current TSDB head
				// precisely because they are actively being dropped at the collector!
				if len(matches) == 0 {
					if fam := matchActiveDropByName(acc.MetricName, ledger.ActiveDrops); fam != "" {
						matches = []FamilyMatch{{FamilyName: fam, Role: inferRoleFromSuffix(acc.MetricName, fam)}}
					}
				}
			}

			for _, m := range matches {
				dropRec, isDropped := ledger.ActiveDrops[m.FamilyName]
				if !isDropped {
					continue
				}
				// For bucket-only drops, only queries targeting `_bucket` cause regret.
				if dropRec.Action == metrics.ActionDropHistogramBuckets && m.Role != "bucket" {
					continue
				}
				// Only count queries after the metric was dropped (or if query has zero timestamp, treat defensively as post-drop).
				if !entry.Timestamp.IsZero() && !dropRec.DroppedAt.IsZero() && entry.Timestamp.Before(dropRec.DroppedAt) {
					continue
				}
				matchedInEntry[m.FamilyName] = struct{}{}
			}
		}

		for fam := range matchedInEntry {
			ht, ok := hits[fam]
			if !ok {
				ht = &hitTracker{}
				hits[fam] = ht
			}
			entryHits := entry.HitCount
			if entryHits <= 0 {
				entryHits = 1
			}
			ht.count += entryHits
			if entry.Timestamp.After(ht.latestTS) || ht.sampleExpr == "" {
				ht.latestTS = entry.Timestamp
				ht.sampleExpr = entry.Query
			}
		}
	}

	var events []metrics.RegretEvent
	for fam, ht := range hits {
		if ht.count < threshold {
			continue
		}
		dropRec := ledger.ActiveDrops[fam]
		ev := metrics.RegretEvent{
			FamilyName:     fam,
			DetectedAt:     now,
			Query:          ht.sampleExpr,
			QueryTimestamp: ht.latestTS,
			HitCount:       ht.count,
			DroppedAt:      dropRec.DroppedAt,
			Tranche:        dropRec.Tranche,
			ActionTaken:    fmt.Sprintf("un-dropped %s, pinned to neverDrop, blocked next tranche (forward-only; previously dropped samples are not recoverable)", fam),
		}
		ledger.RecordRegret(ev)
		events = append(events, ev)
	}
	return events
}

func matchActiveDropByName(metricName string, active map[string]*state.ActiveDropRecord) string {
	if _, ok := active[metricName]; ok {
		return metricName
	}
	for _, suf := range []string{"_bucket", "_sum", "_count", "_created"} {
		if strings.HasSuffix(metricName, suf) {
			base := strings.TrimSuffix(metricName, suf)
			if _, ok := active[base]; ok {
				return base
			}
		}
	}
	return ""
}

func inferRoleFromSuffix(metricName, base string) string {
	switch metricName {
	case base + "_bucket":
		return "bucket"
	case base + "_sum":
		return "sum"
	case base + "_count":
		return "count"
	case base + "_created":
		return "created"
	default:
		return "base"
	}
}
