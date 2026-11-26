package engine

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/usage/alertmanager"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/k8sconsumers"
	"trimtab.io/trimtab/pkg/usage/perses"
	"trimtab.io/trimtab/pkg/usage/promrules"
	"trimtab.io/trimtab/pkg/usage/querylog"
	"trimtab.io/trimtab/pkg/usage/ruler"
	"trimtab.io/trimtab/pkg/usage/slo"
)

// AnalyzeInput bundles all discovered inputs for a single deterministic engine evaluation.
type AnalyzeInput struct {
	Config              *config.Config
	Ledger              *state.Ledger
	Inventory           *prometheus.InventorySnapshot
	CollapseEvaluator   prometheus.CollapseEvaluator
	QueryLog            *querylog.Result
	Grafana             *grafana.ScanResult
	PromRules           *promrules.ScanResult
	Ruler               *ruler.ScanResult
	K8sConsumers        *k8sconsumers.ScanResult
	Alertmanager        *alertmanager.ScanResult
	Perses              *perses.ScanResult
	SLO                 *slo.ScanResult
	Capabilities        metrics.Capabilities
	TargetName          string
	Now                 time.Time
	AcceptBaselineShift bool
	ActivateNextTranche bool
	PreviewNextTranche  bool
	ConfirmTranche      bool
	ResolveRegret       bool
	HardFetchErrors     []string
}

// Analyze executes the full Trimtab safety-gated correlation, DAG closure, regret check,
// tranche planning, and advisory label-collapse pipeline.
func Analyze(ctx context.Context, in AnalyzeInput) (*metrics.AuditReport, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("Analyze requires non-nil Config")
	}
	in.Config.ApplyDefaults()
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ledger := in.Ledger
	if ledger == nil {
		ledger = state.NewLedger()
	}
	targetName := in.TargetName
	if targetName == "" {
		targetName = in.Config.Synthesizer.Target
	}

	report := &metrics.AuditReport{
		Name:             in.Config.Name,
		GeneratedAt:      now,
		WindowDays:       in.Config.Policy.MinObservationWindow.Days(),
		SoakWindow:       in.Config.Policy.SoakWindow.Duration(),
		Target:           targetName,
		ProtectedReasons: make(map[string]string),
	}
	report.Safety.RequiredWindowDays = in.Config.Policy.MinObservationWindow.Days()
	report.Summary.MaxDropPercentPerTranche = in.Config.Policy.MaxDropPercentPerTranche

	if in.Inventory != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.Inventory.Warnings...)
	}
	if in.QueryLog != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.QueryLog.Warnings...)
	}
	if in.Grafana != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.Grafana.Warnings...)
	}
	if in.PromRules != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.PromRules.Warnings...)
	}
	if in.Ruler != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.Ruler.Warnings...)
	}
	if in.K8sConsumers != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.K8sConsumers.Warnings...)
	}
	if in.Alertmanager != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.Alertmanager.Warnings...)
	}
	if in.Perses != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.Perses.Warnings...)
	}
	if in.SLO != nil {
		report.Safety.Warnings = append(report.Safety.Warnings, in.SLO.Warnings...)
	}

	// 1. Group raw metrics into Metric Families (Invariant #7)
	cat := GroupFamilies(in.Inventory)
	totalFamilies := len(cat.OrderedNames)
	totalSeries := 0
	for _, famName := range cat.OrderedNames {
		f := cat.Families[famName]
		totalSeries += f.Stats.TotalSeries
		// Record observation timestamps in State Ledger (Invariant #5)
		firstObs := f.FirstSeenAt
		if firstObs.IsZero() {
			firstObs = now
		}
		sf := ledger.RecordSeenFamily(famName, firstObs, now)
		f.FirstSeenAt = sf.FirstSeenAt
		f.LastSeenAt = sf.LastSeenAt
		f.LastQueriedAt = sf.LastQueriedAt
		f.ObservedDuration = now.Sub(sf.FirstSeenAt)
	}
	report.Summary.TotalFamilies = totalFamilies
	report.Summary.TotalActiveSeries = totalSeries

	minObsDur := in.Config.Policy.MinObservationWindow.Duration()

	// 2. Update Query-Log Coverage in State Ledger (Invariant #1)
	if in.QueryLog != nil && in.QueryLog.Configured {
		report.Safety.QueryLogConfigured = true
		ledger.UpdateQueryLogCoverage(
			in.QueryLog.ObservedDates,
			in.QueryLog.EarliestTimestamp,
			in.QueryLog.LatestTimestamp,
			in.QueryLog.APIUserCount,
			in.QueryLog.RuleEvalCount,
			in.QueryLog.ExcludedCount,
			now,
			minObsDur,
		)
		if minObsDur > 0 && minObsDur < 24*time.Hour {
			report.Safety.QueryLogCoveredDays = math.Round(ledger.QueryLogCoverage.CoveredDays*10000) / 10000
		} else {
			report.Safety.QueryLogCoveredDays = math.Round(ledger.QueryLogCoverage.CoveredDays*10) / 10
		}
		report.Safety.APIUserQueries = in.QueryLog.APIUserCount
		report.Safety.RuleEvalQueries = in.QueryLog.RuleEvalCount
		report.Safety.ExcludedQueries = in.QueryLog.ExcludedCount
	}

	// 3. Forward-Only Regret Detection FIRST (Invariant #10)
	// Running regret before planning immediately removes regretted families from active_drops
	// and pins them to ledger.PinnedNeverDrop.
	regrets := DetectRegrets(cat, ledger, in.QueryLog, in.Config.Policy, now)
	report.Regrets = regrets

	// Track whether reverted families have reappeared in inventory, and handle explicit --resolve-regret.
	if lastT := ledger.LatestTranche(); lastT != nil {
		if len(lastT.PendingRevertFamilies) > 0 {
			remaining := make([]string, 0, len(lastT.PendingRevertFamilies))
			for _, fam := range lastT.PendingRevertFamilies {
				if isFamilyRestoredInInventory(cat, fam) {
					continue
				}
				remaining = append(remaining, fam)
			}
			lastT.PendingRevertFamilies = remaining
		}
		if in.ResolveRegret && len(regrets) == 0 {
			if len(lastT.PendingRevertFamilies) > 0 {
				report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
					"regret block resolved via --resolve-regret before revert was confirmed in inventory for %v",
					lastT.PendingRevertFamilies,
				))
			}
			ledger.ResolveLatestTrancheRegrets(now)
		}
		report.Safety.UnresolvedRegrets = lastT.UnresolvedRegrets
		if len(lastT.PendingRevertFamilies) > 0 {
			report.Safety.PendingRevertFamilies = append([]string(nil), lastT.PendingRevertFamilies...)
		}
		report.Safety.ActiveTrancheStatus = lastT.Status
	}
	if len(regrets) > 0 && report.Safety.UnresolvedRegrets < len(regrets) {
		report.Safety.UnresolvedRegrets = len(regrets)
	}

	// 4. Hard fetch errors abort synthesis (Invariant #4)
	if len(in.HardFetchErrors) > 0 {
		report.Safety.Blocked = true
		for _, e := range in.HardFetchErrors {
			report.Safety.BlockReasons = append(report.Safety.BlockReasons, "hard fetch error: "+e)
		}
	}

	// 5. Baseline Drift Circuit Breaker (Invariant #4)
	currCounts := collectSourceCounts(in)
	drifted, driftDetails := CheckBaselineDrift(
		ledger.BaselineCounts,
		currCounts,
		in.Config.Policy.BaselineDriftThresholdPercent,
		in.Config.Policy.QueryLogBaselineDriftThresholdPercent,
		in.AcceptBaselineShift,
	)
	report.Safety.BaselineDriftDetails = driftDetails
	if drifted {
		report.Safety.BaselineDriftDetected = true
		report.Safety.Blocked = true
		for _, d := range driftDetails {
			report.Safety.BlockReasons = append(report.Safety.BlockReasons, "baseline drift guard: "+d+" (pass --accept-baseline-shift to acknowledge)")
		}
	} else if len(in.HardFetchErrors) == 0 {
		UpdateBaselineCounts(ledger, currCounts, in.AcceptBaselineShift)
	}

	// 6. Cold-Start / Query-Log Coverage Gate (Invariant #1)
	requiredDays := in.Config.Policy.MinObservationWindow.Days()
	if !report.Safety.QueryLogConfigured {
		report.Safety.ReportOnly = true
		report.Safety.BlockReasons = append(report.Safety.BlockReasons, "no runtime query log configured — staying in Report-Only mode (projected savings only)")
		report.Summary.Confidence = metrics.ConfidenceMedium
	} else if ledger.QueryLogCoverage.CoveredDays+1e-6 < requiredDays {
		report.Safety.ReportOnly = true
		if minObsDur > 0 && minObsDur < 24*time.Hour {
			coveredDur := time.Duration(ledger.QueryLogCoverage.CoveredDays * float64(24*time.Hour))
			report.Safety.BlockReasons = append(report.Safety.BlockReasons, fmt.Sprintf(
				"query log coverage %s/%s < minObservationWindow — staying in Report-Only mode (projected savings only)",
				coveredDur.Round(time.Second), minObsDur,
			))
		} else {
			report.Safety.BlockReasons = append(report.Safety.BlockReasons, fmt.Sprintf(
				"query log coverage %.1f/%.0f days < minObservationWindow — staying in Report-Only mode (projected savings only)",
				ledger.QueryLogCoverage.CoveredDays, requiredDays,
			))
		}
		report.Summary.Confidence = metrics.ConfidenceMedium
	} else {
		report.Summary.Confidence = metrics.ConfidenceHigh
	}

	// 7. Collect all UsageRefs, detect Wildcard Protectors, and filter ExcludeSources (Invariant #6)
	var allRefs []metrics.UsageRef
	if in.QueryLog != nil {
		allRefs = append(allRefs, in.QueryLog.Refs...)
	}
	if in.Grafana != nil {
		allRefs = append(allRefs, in.Grafana.Refs...)
	}
	if in.PromRules != nil {
		allRefs = append(allRefs, in.PromRules.AlertRefs...)
	}
	if in.Ruler != nil {
		allRefs = append(allRefs, in.Ruler.AlertRefs...)
	}
	if in.K8sConsumers != nil {
		allRefs = append(allRefs, in.K8sConsumers.Refs...)
	}
	if in.Perses != nil {
		allRefs = append(allRefs, in.Perses.Refs...)
	}
	if in.SLO != nil {
		allRefs = append(allRefs, in.SLO.Refs...)
	}

	wildcards, excludedSources := detectWildcardsAndExclusions(cat, allRefs, in.Config.Policy)
	report.WildcardSources = wildcards
	for _, ws := range wildcards {
		if !ws.Excluded {
			msg := fmt.Sprintf(
				"wildcard protector detected: %s (%s) matches %d/%d families (%.1f%%); add to policy.excludeSources if intentional explorer",
				ws.SourceName, ws.SourceID, ws.MatchedFamilies, ws.TotalFamilies, ws.MatchPercent,
			)
			if ws.RecommendedExclude != "" {
				msg += " (" + ws.RecommendedExclude + ")"
			}
			report.Safety.Warnings = append(report.Safety.Warnings, msg)
		}
	}

	// 8. Build UsageIndex from non-excluded UsageRefs + ledger-retained query timestamps (log-rotation survival)
	usageIdx := metrics.NewUsageIndex()
	for _, ref := range allRefs {
		if (ref.Origin != metrics.OriginAPIUser && excludedSources[ref.SourceID]) || IsSourceExcluded(ref, in.Config.Policy.ExcludeSources) {
			continue
		}
		matches := cat.MatchRef(ref)
		if len(matches) == 0 && !ref.IsRegex && ref.MetricName != "" {
			// Record direct live references to recording rules that might not be in TSDB inventory.
			fu := usageIdx.GetOrCreate(ref.MetricName)
			recordUsageRef(fu, ref, "base", ledger)
			continue
		}
		for _, m := range matches {
			fu := usageIdx.GetOrCreate(m.FamilyName)
			recordUsageRef(fu, ref, m.Role, ledger)
		}
	}

	// Hydrate recent query usage from State Ledger so log rotation does not erase usage within minObservationWindow.
	windowStart := now.Add(-minObsDur)
	for famName, sf := range ledger.SeenFamilies {
		if sf == nil {
			continue
		}
		if !sf.LastQueriedAt.IsZero() && !sf.LastQueriedAt.Before(windowStart) && !sf.LastQueriedAt.After(now) {
			fu := usageIdx.GetOrCreate(famName)
			fu.LiveFromLedger = true
			if sf.LastQueriedAt.After(fu.LastLiveQueryAt) {
				fu.LastLiveQueryAt = sf.LastQueriedAt
			}
		}
		if !sf.LastBucketQueriedAt.IsZero() && !sf.LastBucketQueriedAt.Before(windowStart) && !sf.LastBucketQueriedAt.After(now) {
			fu := usageIdx.GetOrCreate(famName)
			fu.LiveFromLedger = true
			fu.UsedBuckets = true
		}
	}

	// Protect alert routing/grouping/template labels referenced by Alertmanager on alerted metrics only.
	applyAlertmanagerLabels(usageIdx, in.Alertmanager)

	// 9. Recording-Rule Transitive DAG & Dead Chain Detection (Invariant #8)
	var recRules []promrules.RecordingRule
	if in.PromRules != nil && len(in.PromRules.RecordingRules) > 0 {
		recRules = append(recRules, in.PromRules.RecordingRules...)
	}
	if in.Ruler != nil && len(in.Ruler.RecordingRules) > 0 {
		recRules = append(recRules, in.Ruler.RecordingRules...)
	}
	dagRes := ResolveRecordingRuleDAG(cat, usageIdx, recRules)
	report.DeadRecordingRules = dagRes.DeadRules

	// 10. Classify each MetricFamily (neverDrop, observation window, live usage, bucket-only, droppable)
	neverDrop, err := NewNeverDropMatcher(in.Config.Policy, ledger)
	if err != nil {
		return nil, err
	}

	var droppableCandidates []metrics.Recommendation
	var existingActiveDrops []metrics.Recommendation
	var coldStartProjected []metrics.Recommendation
	protectedCount := 0

	for _, famName := range cat.OrderedNames {
		f := cat.Families[famName]
		fu := usageIdx.Families[famName]

		// Refresh LastQueriedAt from ledger
		if sf, ok := ledger.SeenFamilies[famName]; ok {
			f.LastQueriedAt = sf.LastQueriedAt
		}

		// Check neverDrop allowlist & regret-pinned list (Invariants #6 & #10)
		if isProt, reason := neverDrop.Match(famName, f.Members); isProt {
			protectedCount++
			report.ProtectedReasons[famName] = reason
			continue
		}

		// Dead recording-rule output series (ruler bypasses collector; emit advisory only)
		if _, isDeadRec := dagRes.DeadRecordedNames[famName]; isDeadRec {
			protectedCount++
			report.ProtectedReasons[famName] = "dead recording rule (ruler bypasses collector — delete in rule files)"
			continue
		}

		// Check live usage
		if fu != nil && fu.IsLive() {
			// Check partial-family classic histogram bucket-only optimization (Invariant #7)
			if f.HasClassicBuckets() && !f.IsNativeHistogram && fu.UsedSumOrCount && !fu.UsedBuckets && f.Stats.BucketSeries > 0 {
				obsDays := math.Round(f.ObservedDuration.Hours()/24.0*10) / 10
				if f.ObservedDuration < minObsDur {
					protectedCount++
					report.ProtectedReasons[famName] = fmt.Sprintf("observed %.1fd < minObservationWindow %.0fd", obsDays, requiredDays)
					if report.Safety.ReportOnly && in.Capabilities.CanDropHistogramBucketsOnly {
						coldStartProjected = append(coldStartProjected, metrics.Recommendation{
							FamilyName:   famName,
							MetricType:   f.Type,
							Action:       metrics.ActionDropHistogramBuckets,
							Confidence:   report.Summary.Confidence,
							Series:       f.Stats.BucketSeries,
							Reason:       fmt.Sprintf("projected (only _sum/_count consumed; observed %s < %s)", formatObservedWindow(f.ObservedDuration, minObsDur), formatObservedWindow(minObsDur, minObsDur)),
							ObservedDays: obsDays,
							Members:      []string{famName + "_bucket"},
							NamingMode:   f.NamingMode,
							OTLPName:     f.OTLPName,
						})
					}
					continue
				}
				rec := metrics.Recommendation{
					FamilyName:   famName,
					MetricType:   f.Type,
					Action:       metrics.ActionDropHistogramBuckets,
					Confidence:   report.Summary.Confidence,
					Series:       f.Stats.BucketSeries,
					Reason:       fmt.Sprintf("only _sum/_count consumed; 0 _bucket consumers (observed %.0fd)", obsDays),
					ObservedDays: obsDays,
					Members:      []string{famName + "_bucket"},
					NamingMode:   f.NamingMode,
					OTLPName:     f.OTLPName,
				}
				if in.Capabilities.CanDropHistogramBucketsOnly {
					if _, already := ledger.ActiveDrops[famName]; already {
						rec.Synthesized = true
						existingActiveDrops = append(existingActiveDrops, rec)
					} else {
						droppableCandidates = append(droppableCandidates, rec)
					}
				} else {
					rec.AdvisoryOnly = true
					rec.AdvisoryNote = fmt.Sprintf("target %q does not support histogram bucket-only drops (whole-instrument filter only); kept as advisory", targetName)
					report.CapabilityAdvisories = append(report.CapabilityAdvisories, rec)
					protectedCount++
					report.ProtectedReasons[famName] = "live (_sum/_count used; target cannot drop _bucket alone)"
				}
				continue
			}

			// Preserve an active bucket-only drop even after `_bucket` disappears from TSDB inventory
			// while `_sum`/`_count` remain present and live.
			if dropRec, already := ledger.ActiveDrops[famName]; already && dropRec.Action == metrics.ActionDropHistogramBuckets && !fu.UsedBuckets && in.Capabilities.CanDropHistogramBucketsOnly {
				members := append([]string(nil), dropRec.Members...)
				if len(members) == 0 {
					members = []string{famName + "_bucket"}
				}
				existingActiveDrops = append(existingActiveDrops, metrics.Recommendation{
					FamilyName:    famName,
					MetricType:    f.Type,
					Action:        dropRec.Action,
					Confidence:    report.Summary.Confidence,
					Series:        dropRec.SeriesAtDrop,
					Reason:        dropRec.Reason,
					TrancheNumber: dropRec.Tranche,
					Synthesized:   true,
					Members:       members,
					NamingMode:    f.NamingMode,
					OTLPName:      f.OTLPName,
				})
				continue
			}

			protectedCount++
			if fu.TransitivelyLive && !fu.HasDirectLiveUsage() {
				report.ProtectedReasons[famName] = fmt.Sprintf("transitively live via recording rule(s): %v", fu.TransitiveLiveVia)
			} else if !fu.HasDirectLiveUsage() && fu.LiveFromLedger {
				report.ProtectedReasons[famName] = "recently queried in ledger within observation window"
			} else {
				report.ProtectedReasons[famName] = "actively consumed"
			}
			continue
		}

		// Unused family: preserve active drops even if current series is 0, ignore non-active
		// historical 0-series names, and check observation window (Invariant #5).
		dropRec, alreadyActive := ledger.ActiveDrops[famName]
		obsDays := math.Round(f.ObservedDuration.Hours()/24.0*10) / 10
		if !alreadyActive {
			if f.Stats.TotalSeries <= 0 {
				protectedCount++
				report.ProtectedReasons[famName] = "0 active series in current inventory"
				continue
			}
			if f.ObservedDuration < minObsDur {
				protectedCount++
				report.ProtectedReasons[famName] = fmt.Sprintf("observed %.1fd < minObservationWindow %.0fd", obsDays, requiredDays)
				if report.Safety.ReportOnly {
					reason := fmt.Sprintf("projected (0 live consumers; observed %s < %s)", formatObservedWindow(f.ObservedDuration, minObsDur), formatObservedWindow(minObsDur, minObsDur))
					if deadRules := dagRes.SourceDeadChains[famName]; len(deadRules) > 0 {
						reason = fmt.Sprintf("projected (dead recording-rule chain, %d rules; observed %s < %s)", len(deadRules), formatObservedWindow(f.ObservedDuration, minObsDur), formatObservedWindow(minObsDur, minObsDur))
					}
					coldStartProjected = append(coldStartProjected, metrics.Recommendation{
						FamilyName:   famName,
						MetricType:   f.Type,
						Action:       metrics.ActionDropFamily,
						Confidence:   report.Summary.Confidence,
						Series:       f.Stats.TotalSeries,
						Reason:       reason,
						ObservedDays: obsDays,
						Members:      f.Members,
						NamingMode:   f.NamingMode,
						OTLPName:     f.OTLPName,
					})
				}
				continue
			}
		}

		// Eligible for DROP_FAMILY!
		series := f.Stats.TotalSeries
		if alreadyActive && series == 0 && dropRec.SeriesAtDrop > 0 {
			series = dropRec.SeriesAtDrop
		}
		reason := fmt.Sprintf("0 live consumers (observed %.0fd)", obsDays)
		if deadRules := dagRes.SourceDeadChains[famName]; len(deadRules) > 0 {
			reason = fmt.Sprintf("dead recording-rule chain (%d rules)", len(deadRules))
		}
		rec := metrics.Recommendation{
			FamilyName:   famName,
			MetricType:   f.Type,
			Action:       metrics.ActionDropFamily,
			Confidence:   report.Summary.Confidence,
			Series:       series,
			Reason:       reason,
			ObservedDays: obsDays,
			Members:      f.Members,
			NamingMode:   f.NamingMode,
			OTLPName:     f.OTLPName,
		}
		if alreadyActive {
			rec.Synthesized = true
			existingActiveDrops = append(existingActiveDrops, rec)
		} else {
			droppableCandidates = append(droppableCandidates, rec)
		}
	}

	// Also preserve previously active drops whose metrics are no longer present in TSDB head
	// specifically BECAUSE the collector is already dropping them!
	for famName, dropRec := range ledger.ActiveDrops {
		if _, inInventory := cat.Families[famName]; inInventory {
			continue
		}
		if isProt, _ := neverDrop.Match(famName, nil); isProt {
			continue
		}
		mt := dropRec.MetricType
		if mt == "" {
			mt = metrics.MetricTypeUnknown
		}
		members := append([]string(nil), dropRec.Members...)
		if len(members) == 0 {
			if dropRec.Action == metrics.ActionDropHistogramBuckets {
				members = []string{famName + "_bucket"}
			} else {
				members = []string{famName}
			}
		}
		nm := dropRec.NamingMode
		if nm == "" {
			nm = metrics.NamingMode(in.Config.Inventory.NamingMode)
		}
		existingActiveDrops = append(existingActiveDrops, metrics.Recommendation{
			FamilyName:    famName,
			MetricType:    mt,
			Action:        dropRec.Action,
			Confidence:    report.Summary.Confidence,
			Series:        dropRec.SeriesAtDrop,
			Reason:        dropRec.Reason,
			TrancheNumber: dropRec.Tranche,
			Synthesized:   true,
			Members:       members,
			NamingMode:    nm,
			OTLPName:      dropRec.OTLPName,
		})
	}

	// Combine existing active drops + new candidates for estate-wide droppable accounting.
	allDroppable := make([]metrics.Recommendation, 0, len(existingActiveDrops)+len(droppableCandidates))
	allDroppable = append(allDroppable, existingActiveDrops...)
	allDroppable = append(allDroppable, droppableCandidates...)

	totalDroppableSeries := 0
	for _, r := range allDroppable {
		totalDroppableSeries += r.Series
	}
	droppablePct := 0.0
	if totalSeries > 0 {
		droppablePct = math.Round(float64(totalDroppableSeries)/float64(totalSeries)*1000) / 10
	}
	report.Summary.DroppableFamilies = len(allDroppable)
	report.Summary.DroppableSeries = totalDroppableSeries
	report.Summary.DroppablePercent = droppablePct
	report.Summary.ProtectedFamilies = protectedCount

	// 11. Sanity-Abort Ceiling Check (Invariant #3: >80% droppable => broken query-log wiring)
	if totalSeries > 0 && droppablePct > in.Config.Policy.MaxTotalDroppablePercent {
		report.Safety.SanityCeilingTripped = true
		report.Safety.Blocked = true
		report.Safety.BlockReasons = append(report.Safety.BlockReasons, fmt.Sprintf(
			"sanity ceiling tripped: %.1f%% of active series appears droppable (> %.1f%% maxTotalDroppablePercent); verify query log wiring",
			droppablePct, in.Config.Policy.MaxTotalDroppablePercent,
		))
	}

	// 12. Tranched Rollout Planning & Soak Gate (Invariant #3)
	// Plan tranches across new candidates (or all droppable on first rollout).
	plannedTranches, oversized := PlanTranches(droppableCandidates, totalSeries, in.Config.Policy.MaxDropPercentPerTranche)
	report.OversizedFamilies = oversized
	if len(oversized) > 0 {
		for _, ov := range oversized {
			report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf("oversized family %s (%d series): %s", ov.FamilyName, ov.Series, ov.AdvisoryNote))
		}
	}

	// Offset tranche numbers after already-activated tranches in ledger.
	priorTranches := len(ledger.Tranches)
	for i := range plannedTranches {
		plannedTranches[i].Number = priorTranches + i + 1
		for j := range plannedTranches[i].Items {
			plannedTranches[i].Items[j].TrancheNumber = plannedTranches[i].Number
		}
	}
	report.Tranches = plannedTranches
	report.Summary.TotalTranches = priorTranches + len(plannedTranches)

	canSynthesize := !report.Safety.ReportOnly && !report.Safety.Blocked
	if !canSynthesize {
		// Report-Only or Blocked: 0 synthesized drops; expose projected drops for visibility!
		projected := append([]metrics.Recommendation(nil), allDroppable...)
		if report.Safety.ReportOnly && len(coldStartProjected) > 0 {
			seenProj := make(map[string]struct{}, len(projected))
			for _, p := range projected {
				seenProj[p.FamilyName] = struct{}{}
			}
			for _, p := range coldStartProjected {
				if _, dup := seenProj[p.FamilyName]; !dup {
					seenProj[p.FamilyName] = struct{}{}
					projected = append(projected, p)
				}
			}
		}
		sort.Slice(projected, func(i, j int) bool {
			if projected[i].Series != projected[j].Series {
				return projected[i].Series > projected[j].Series
			}
			return projected[i].FamilyName < projected[j].FamilyName
		})
		report.ProjectedDrops = projected
		report.Summary.SynthesizedFamilies = 0
		report.Summary.SynthesizedSeries = 0
		report.Summary.SynthesizedPercent = 0
		if len(plannedTranches) > 0 {
			report.Summary.ActiveTranche = 0
		}
	} else {
		// Evaluate latest tranche lifecycle:
		// pending_confirmation -> soaking -> soaked (or blocked_regret).
		canUnlockNext := true
		if lastT := ledger.LatestTranche(); lastT != nil {
			if lastT.UnresolvedRegrets > 0 {
				canUnlockNext = false
				if len(lastT.PendingRevertFamilies) > 0 {
					report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
						"tranche %d has %d unresolved regret(s) awaiting collector revert for %v (apply reverted rules then run --resolve-regret); holding next tranche rollout",
						lastT.Number, lastT.UnresolvedRegrets, lastT.PendingRevertFamilies,
					))
				} else {
					report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
						"tranche %d has %d unresolved regret(s) with revert confirmed in inventory; run --resolve-regret to unblock next tranche rollout",
						lastT.Number, lastT.UnresolvedRegrets,
					))
				}
			} else if lastT.Status == "pending_confirmation" {
				confirmed := in.ConfirmTranche ||
					in.Config.Policy.SoakStartMode == "immediate" ||
					isTrancheConfirmedInInventory(lastT, ledger, cat, in.Config.Policy.ConfirmationMaxRemainingPercent)
				if confirmed {
					ledger.ConfirmLatestTranche(now, in.Config.Policy.SoakWindow.Duration())
					if now.Before(lastT.SoakUntil) {
						canUnlockNext = false
						report.Safety.SoakBlocked = true
						report.Safety.SoakRemaining = lastT.SoakUntil.Sub(now)
					} else {
						lastT.Status = "soaked"
					}
				} else {
					canUnlockNext = false
					report.Safety.PendingConfirmation = true
					activatedAt := lastT.ActivatedAt
					if activatedAt.IsZero() {
						activatedAt = lastT.StartedAt
					}
					report.Safety.PendingConfirmationSince = activatedAt
					if !activatedAt.IsZero() && now.Sub(activatedAt) > 48*time.Hour {
						report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
							"tranche %d has remained pending_confirmation for >48h (activated %s); verify collector rules are deployed or pass --confirm-tranche",
							lastT.Number, activatedAt.UTC().Format(time.RFC3339),
						))
					}
				}
			} else if lastT.Status == "soaking" || lastT.Status == "active" {
				if now.Before(lastT.SoakUntil) {
					canUnlockNext = false
					report.Safety.SoakBlocked = true
					report.Safety.SoakRemaining = lastT.SoakUntil.Sub(now)
				} else {
					lastT.Status = "soaked"
				}
			} else if lastT.Status != "soaked" {
				canUnlockNext = false
			}
		}

		activeDrops := append([]metrics.Recommendation(nil), existingActiveDrops...)
		activeTrancheNum := priorTranches
		if canUnlockNext && len(plannedTranches) > 0 {
			nextTranche := plannedTranches[0]
			if in.ActivateNextTranche {
				activeTrancheNum = nextTranche.Number
				for i := range nextTranche.Items {
					nextTranche.Items[i].Synthesized = true
					activeDrops = append(activeDrops, nextTranche.Items[i])
				}
				immediate := in.Config.Policy.SoakStartMode == "immediate"
				ledger.RecordTrancheActivation(nextTranche.Number, now, in.Config.Policy.SoakWindow.Duration(), immediate, nextTranche.Items)
				if lastT := ledger.LatestTranche(); lastT != nil && lastT.Status == "pending_confirmation" {
					report.Safety.PendingConfirmation = true
					report.Safety.PendingConfirmationSince = lastT.ActivatedAt
				}
			} else if in.PreviewNextTranche {
				activeTrancheNum = nextTranche.Number
				for i := range nextTranche.Items {
					nextTranche.Items[i].Synthesized = true
					activeDrops = append(activeDrops, nextTranche.Items[i])
				}
			} else {
				// Observation-only analyze or plain synth without --activate: never auto-activate new tranches.
				report.Safety.NextTrancheReady = true
			}
		}
		if lastT := ledger.LatestTranche(); lastT != nil {
			report.Safety.ActiveTrancheStatus = lastT.Status
		}

		sort.Slice(activeDrops, func(i, j int) bool {
			if activeDrops[i].Series != activeDrops[j].Series {
				return activeDrops[i].Series > activeDrops[j].Series
			}
			return activeDrops[i].FamilyName < activeDrops[j].FamilyName
		})

		synthSeries := 0
		for _, d := range activeDrops {
			synthSeries += d.Series
		}
		report.ActiveDrops = activeDrops
		report.Summary.ActiveTranche = activeTrancheNum
		report.Summary.SynthesizedFamilies = len(activeDrops)
		report.Summary.SynthesizedSeries = synthSeries
		if totalSeries > 0 {
			report.Summary.SynthesizedPercent = math.Round(float64(synthSeries)/float64(totalSeries)*1000) / 10
		}
	}
	report.Summary.ActiveDropWatchCount = len(ledger.ActiveDrops)

	// 13. Advisory High-Cardinality Unused Labels (`without`-based collapse, Invariant #9)
	if in.Config.Inventory.CollapseQueriesEnabled() && in.CollapseEvaluator != nil {
		report.LabelCollapseAdvisories = computeLabelCollapseAdvisories(
			ctx, cat, usageIdx, in.CollapseEvaluator, in.Config.Inventory.MaxCollapseFamilies,
		)
	}

	ledger.UpdatedAt = now
	return report, nil
}

func isTrancheConfirmedInInventory(
	tr *state.TrancheRecord,
	ledger *state.Ledger,
	cat *FamilyCatalog,
	maxRemainingPct float64,
) bool {
	if tr == nil {
		return false
	}
	totalOriginal := 0
	totalRemaining := 0
	for _, famName := range tr.Families {
		dropRec, ok := ledger.ActiveDrops[famName]
		if !ok {
			continue
		}
		totalOriginal += dropRec.SeriesAtDrop
		f, inInv := cat.Families[famName]
		if !inInv {
			continue
		}
		if dropRec.Action == metrics.ActionDropHistogramBuckets {
			totalRemaining += f.Stats.BucketSeries
		} else {
			totalRemaining += f.Stats.TotalSeries
		}
	}
	if maxRemainingPct <= 0 {
		return totalRemaining == 0
	}
	if totalOriginal <= 0 {
		return totalRemaining == 0
	}
	remainingPct := float64(totalRemaining) / float64(totalOriginal) * 100.0
	return remainingPct <= maxRemainingPct
}

func collectSourceCounts(in AnalyzeInput) SourceCounts {
	var sc SourceCounts
	if in.Grafana != nil && in.Config.Usage.Grafana != nil {
		sc.Dashboards = state.IntPtr(in.Grafana.DashboardCount)
		if in.Config.Usage.Grafana.ManagedAlertsEnabled() && (in.Config.Usage.Grafana.URL != "" || in.Config.Usage.Grafana.AlertRulesPath != "") {
			sc.GrafanaAlerts = state.IntPtr(in.Grafana.AlertRuleCount)
		}
	}
	if in.PromRules != nil && len(in.Config.Usage.PromRules) > 0 {
		sc.PromRules = state.IntPtr(in.PromRules.TotalRuleCount)
	}
	if in.Ruler != nil && len(in.Config.Usage.Rulers) > 0 {
		sc.RulerRules = state.IntPtr(in.Ruler.TotalRuleCount)
	}
	if in.K8sConsumers != nil && in.Config.Usage.K8sConsumers != nil {
		kc := in.Config.Usage.K8sConsumers
		if kc.KEDAEnabled() {
			sc.KEDATriggers = state.IntPtr(in.K8sConsumers.KEDATriggerCount)
		}
		if kc.PromAdapterEnabled() {
			sc.PromAdapterRules = state.IntPtr(in.K8sConsumers.PromAdapterRuleCount)
		}
		if kc.ArgoRolloutsEnabled() {
			sc.RolloutTemplates = state.IntPtr(in.K8sConsumers.RolloutTemplateCount)
		}
		if kc.FlaggerEnabled() {
			sc.FlaggerTemplates = state.IntPtr(in.K8sConsumers.FlaggerTemplateCount)
		}
	}
	if in.Alertmanager != nil && in.Config.Usage.Alertmanager != nil {
		sc.AlertmanagerMatchers = state.IntPtr(in.Alertmanager.MatcherCount)
	}
	if in.Perses != nil && in.Config.Usage.Perses != nil {
		sc.PersesDashboards = state.IntPtr(in.Perses.DashboardCount)
	}
	if in.SLO != nil && in.Config.Usage.SLO != nil {
		sc.SLOs = state.IntPtr(in.SLO.SLOCount)
	}
	if in.QueryLog != nil && in.QueryLog.Configured {
		sc.QueryLogEntries = state.IntPtr(in.QueryLog.TotalEntries)
		numerator := in.QueryLog.APIUserCount + in.QueryLog.RuleEvalCount
		if numerator == 0 && in.QueryLog.ExcludedCount == 0 {
			numerator = in.QueryLog.TotalEntries
		}
		var rate float64
		if numerator > 0 {
			winDays := in.QueryLog.RequestedWindow.Hours() / 24.0
			if len(in.QueryLog.ObservedDates) > 0 {
				days := 0
				for _, cnt := range in.QueryLog.ObservedDates {
					if cnt > 0 {
						days++
					}
				}
				if days > 0 {
					denom := float64(days)
					if winDays > 0 && winDays < denom {
						denom = winDays
					}
					rate = math.Round(float64(numerator)/denom*100) / 100
				}
			} else {
				days := 1.0
				if winDays > 0 && winDays < days {
					days = winDays
				} else if !in.QueryLog.EarliestTimestamp.IsZero() && !in.QueryLog.LatestTimestamp.IsZero() {
					d := in.QueryLog.LatestTimestamp.Sub(in.QueryLog.EarliestTimestamp).Hours() / 24.0
					if d > 1.0 {
						days = d
					}
				}
				rate = math.Round(float64(numerator)/days*100) / 100
			}
		}
		sc.QueryLogDailyRate = state.Float64Ptr(rate)
	}
	return sc
}

func isFamilyRestoredInInventory(cat *FamilyCatalog, family string) bool {
	if cat == nil {
		return false
	}
	f, ok := cat.Families[family]
	if !ok || f == nil {
		return false
	}
	if f.Type == metrics.MetricTypeHistogram && !f.IsNativeHistogram {
		return f.Stats.BucketSeries > 0
	}
	return f.Stats.TotalSeries > 0
}

func formatObservedWindow(d, ref time.Duration) string {
	if ref > 0 && ref < 24*time.Hour {
		if d < 0 {
			d = 0
		}
		return d.Round(time.Second).String()
	}
	return fmt.Sprintf("%.0fd", math.Round(d.Hours()/24.0*10)/10)
}

func detectWildcardsAndExclusions(cat *FamilyCatalog, refs []metrics.UsageRef, policy config.PolicyConfig) ([]metrics.WildcardSource, map[string]bool) {
	totalFamilies := len(cat.OrderedNames)
	excludedSources := make(map[string]bool)
	if totalFamilies == 0 {
		return nil, excludedSources
	}

	type sourceAgg struct {
		sourceID     string
		sourceName   string
		origin       metrics.UsageOrigin
		dashboardUID string
		query        string
		matched      map[string]struct{}
		excluded     bool
	}
	bySource := make(map[string]*sourceAgg)

	for _, ref := range refs {
		if ref.Origin == metrics.OriginRuleEval {
			continue
		}

		// For runtime query logs, evaluate wildcards strictly per individual regex expression
		// so shared dashboard servers or caller IPs issuing diverse exact queries are never flagged.
		if ref.Origin == metrics.OriginAPIUser {
			if !ref.IsRegex {
				continue
			}
			key := "querylog:expr:" + ref.Query
			name := fmt.Sprintf("query-log expr %q", ref.Query)
			if ref.Query == "" {
				key = "querylog:regex:" + ref.MetricRegex
				name = fmt.Sprintf("query-log regex %q", ref.MetricRegex)
			}
			sa, ok := bySource[key]
			if !ok {
				sa = &sourceAgg{
					sourceID:     key,
					sourceName:   name,
					origin:       ref.Origin,
					dashboardUID: ref.DashboardUID,
					query:        ref.Query,
					matched:      make(map[string]struct{}),
				}
				bySource[key] = sa
			} else if sa.dashboardUID == "" && ref.DashboardUID != "" {
				sa.dashboardUID = ref.DashboardUID
			}
			if IsSourceExcluded(ref, policy.ExcludeSources) {
				sa.excluded = true
			}
			for _, m := range cat.MatchRef(ref) {
				sa.matched[m.FamilyName] = struct{}{}
			}
			continue
		}

		sa, ok := bySource[ref.SourceID]
		if !ok {
			sa = &sourceAgg{
				sourceID:   ref.SourceID,
				sourceName: ref.SourceName,
				origin:     ref.Origin,
				matched:    make(map[string]struct{}),
			}
			bySource[ref.SourceID] = sa
		}
		if IsSourceExcluded(ref, policy.ExcludeSources) {
			sa.excluded = true
			excludedSources[ref.SourceID] = true
		}
		for _, m := range cat.MatchRef(ref) {
			sa.matched[m.FamilyName] = struct{}{}
		}
	}

	var wildcards []metrics.WildcardSource
	for _, sa := range bySource {
		pct := float64(len(sa.matched)) / float64(totalFamilies) * 100.0
		if pct > policy.WildcardThresholdPercent {
			recExclude := ""
			switch {
			case sa.origin == metrics.OriginAPIUser && sa.dashboardUID != "" && sa.query != "":
				recExclude = fmt.Sprintf("recommended exclude: dashboard_uid=%q or query_regex=%q", sa.dashboardUID, "^"+regexp.QuoteMeta(sa.query)+"$")
			case sa.origin == metrics.OriginAPIUser && sa.query != "":
				recExclude = fmt.Sprintf("recommended exclude: query_regex=%q", "^"+regexp.QuoteMeta(sa.query)+"$")
			case sa.origin == metrics.OriginGrafanaDashboard && strings.HasPrefix(sa.sourceID, "grafana-dashboard:"):
				recExclude = fmt.Sprintf("recommended exclude: dashboard_uid=%q", strings.TrimPrefix(sa.sourceID, "grafana-dashboard:"))
			case sa.origin == metrics.OriginPersesDashboard && strings.HasPrefix(sa.sourceID, "perses-dashboard:"):
				recExclude = fmt.Sprintf("recommended exclude: dashboard_uid=%q", strings.TrimPrefix(sa.sourceID, "perses-dashboard:"))
			}
			wildcards = append(wildcards, metrics.WildcardSource{
				SourceID:           sa.sourceID,
				SourceName:         sa.sourceName,
				Origin:             sa.origin,
				MatchedFamilies:    len(sa.matched),
				TotalFamilies:      totalFamilies,
				MatchPercent:       math.Round(pct*10) / 10,
				Excluded:           sa.excluded,
				RecommendedExclude: recExclude,
			})
		}
	}
	sort.Slice(wildcards, func(i, j int) bool {
		if wildcards[i].MatchPercent != wildcards[j].MatchPercent {
			return wildcards[i].MatchPercent > wildcards[j].MatchPercent
		}
		return wildcards[i].SourceID < wildcards[j].SourceID
	})
	return wildcards, excludedSources
}

func applyAlertmanagerLabels(usageIdx *metrics.UsageIndex, am *alertmanager.ScanResult) {
	if am == nil || (!am.PreservesAllLabels && len(am.ProtectedLabels) == 0) {
		return
	}
	for _, fu := range usageIdx.Families {
		if fu == nil || !hasAlertLiveRef(fu) {
			continue
		}
		if am.PreservesAllLabels {
			fu.Labels.MarkPreservesAll()
		}
		fu.Labels.MarkExplicit(am.ProtectedLabels...)
	}
}

func hasAlertLiveRef(fu *metrics.FamilyUsage) bool {
	for _, ref := range fu.LiveRefs {
		switch ref.Origin {
		case metrics.OriginPromAlert, metrics.OriginGrafanaAlert, metrics.OriginRulerAlert:
			return true
		}
	}
	return false
}

func recordUsageRef(fu *metrics.FamilyUsage, ref metrics.UsageRef, role string, ledger *state.Ledger) {
	if ref.Origin == metrics.OriginRuleEval {
		// Invariant #2: rule-evals NEVER mark a metric live-used!
		fu.RuleEvalRefs = append(fu.RuleEvalRefs, ref)
		fu.RuleEvalQueryCount++
		return
	}
	if !ref.Origin.IsLiveSink() {
		return
	}

	fu.LiveRefs = append(fu.LiveRefs, ref)
	if ref.Origin == metrics.OriginAPIUser {
		fu.APIUserQueryCount++
	}
	if !ref.Timestamp.IsZero() {
		if ref.Timestamp.After(fu.LastLiveQueryAt) {
			fu.LastLiveQueryAt = ref.Timestamp
		}
		ledger.RecordLastQueried(fu.FamilyName, ref.Timestamp)
		if role == "bucket" {
			ledger.RecordLastBucketQueried(fu.FamilyName, ref.Timestamp)
		}
	}
	applyRoleUsage(fu, role)

	if ref.PreservesAll {
		fu.Labels.MarkPreservesAll()
	} else if len(ref.WithoutLabels) > 0 {
		fu.Labels.MarkWithout(ref.WithoutLabels)
	}
	fu.Labels.MarkExplicit(ref.ExplicitLabels...)
}

func computeLabelCollapseAdvisories(
	ctx context.Context,
	cat *FamilyCatalog,
	usageIdx *metrics.UsageIndex,
	eval prometheus.CollapseEvaluator,
	maxFamilies int,
) []metrics.LabelCollapseAdvisory {
	// Evaluate live families sorted by descending series count (bounded by maxFamilies).
	type candidate struct {
		fam          *metrics.MetricFamily
		targetMetric string
		targetSeries int
		unusedLabels []string
	}
	var cands []candidate
	for _, famName := range cat.OrderedNames {
		f := cat.Families[famName]
		fu := usageIdx.Families[famName]
		if fu == nil || (!fu.HasDirectLiveUsage() && !fu.TransitivelyLive) || len(f.Stats.Labels) == 0 || f.Stats.TotalSeries <= 0 {
			continue
		}
		unused := fu.Labels.UnusedLabels(f.Stats.Labels)
		if len(unused) == 0 {
			continue
		}
		// Pick highest-cardinality member (e.g., _bucket for classic histograms).
		targetMetric := f.Name
		targetSeries := f.Stats.TotalSeries
		bestMemberSeries := -1
		for _, m := range f.Members {
			if s := f.Stats.MemberSeries[m]; s > bestMemberSeries {
				bestMemberSeries = s
				targetMetric = m
				targetSeries = s
			}
		}
		cands = append(cands, candidate{
			fam:          f,
			targetMetric: targetMetric,
			targetSeries: targetSeries,
			unusedLabels: unused,
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].targetSeries != cands[j].targetSeries {
			return cands[i].targetSeries > cands[j].targetSeries
		}
		return cands[i].fam.Name < cands[j].fam.Name
	})
	if maxFamilies > 0 && len(cands) > maxFamilies {
		cands = cands[:maxFamilies]
	}

	var advisories []metrics.LabelCollapseAdvisory
	for _, c := range cands {
		combined, singles, err := eval.EstimateCollapse(ctx, c.targetMetric, c.unusedLabels, c.targetSeries)
		if err != nil || (combined == nil && len(singles) == 0) {
			continue
		}
		advisories = append(advisories, metrics.LabelCollapseAdvisory{
			FamilyName:           c.fam.Name,
			TargetMetric:         c.targetMetric,
			UnusedLabels:         c.unusedLabels,
			CombinedEstimate:     combined,
			SingleLabelEstimates: singles,
		})
	}
	return advisories
}
