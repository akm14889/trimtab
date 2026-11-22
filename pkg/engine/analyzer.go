package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/state"
	"trimtab.io/trimtab/pkg/usage/grafana"
	"trimtab.io/trimtab/pkg/usage/promrules"
	"trimtab.io/trimtab/pkg/usage/querylog"
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
	Capabilities        metrics.Capabilities
	TargetName          string
	Now                 time.Time
	AcceptBaselineShift bool
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

	// 2. Update Query-Log Coverage in State Ledger (Invariant #1)
	if in.QueryLog != nil && in.QueryLog.Configured {
		report.Safety.QueryLogConfigured = true
		ledger.UpdateQueryLogCoverage(
			in.QueryLog.EarliestTimestamp,
			in.QueryLog.LatestTimestamp,
			in.QueryLog.APIUserCount,
			in.QueryLog.RuleEvalCount,
			in.QueryLog.ExcludedCount,
		)
		report.Safety.QueryLogCoveredDays = math.Round(ledger.QueryLogCoverage.CoveredDays*10) / 10
		report.Safety.APIUserQueries = in.QueryLog.APIUserCount
		report.Safety.RuleEvalQueries = in.QueryLog.RuleEvalCount
		report.Safety.ExcludedQueries = in.QueryLog.ExcludedCount
	}

	// 3. Forward-Only Regret Detection FIRST (Invariant #10)
	// Running regret before planning immediately removes regretted families from active_drops
	// and pins them to ledger.PinnedNeverDrop.
	regrets := DetectRegrets(cat, ledger, in.QueryLog, in.Config.Policy, now)
	report.Regrets = regrets
	if lastT := ledger.LatestTranche(); lastT != nil {
		report.Safety.UnresolvedRegrets = lastT.UnresolvedRegrets
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
		UpdateBaselineCounts(ledger, currCounts)
	}

	// 6. Cold-Start / Query-Log Coverage Gate (Invariant #1)
	requiredDays := in.Config.Policy.MinObservationWindow.Days()
	if !report.Safety.QueryLogConfigured {
		report.Safety.ReportOnly = true
		report.Safety.BlockReasons = append(report.Safety.BlockReasons, "no runtime query log configured — staying in Report-Only mode (projected savings only)")
		report.Summary.Confidence = metrics.ConfidenceMedium
	} else if ledger.QueryLogCoverage.CoveredDays+1e-6 < requiredDays {
		report.Safety.ReportOnly = true
		report.Safety.BlockReasons = append(report.Safety.BlockReasons, fmt.Sprintf(
			"query log coverage %.1f/%.0f days < minObservationWindow — staying in Report-Only mode (projected savings only)",
			ledger.QueryLogCoverage.CoveredDays, requiredDays,
		))
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

	wildcards, excludedSources := detectWildcardsAndExclusions(cat, allRefs, in.Config.Policy)
	report.WildcardSources = wildcards
	for _, ws := range wildcards {
		if !ws.Excluded {
			report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
				"wildcard protector detected: %s (%s) matches %d/%d families (%.1f%%); add to policy.excludeSources if intentional explorer",
				ws.SourceName, ws.SourceID, ws.MatchedFamilies, ws.TotalFamilies, ws.MatchPercent,
			))
		}
	}

	// 8. Build UsageIndex from non-excluded UsageRefs
	usageIdx := metrics.NewUsageIndex()
	for _, ref := range allRefs {
		if excludedSources[ref.SourceID] || IsSourceExcluded(ref, in.Config.Policy.ExcludeSources) {
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

	// 9. Recording-Rule Transitive DAG & Dead Chain Detection (Invariant #8)
	var recRules []promrules.RecordingRule
	if in.PromRules != nil {
		recRules = in.PromRules.RecordingRules
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
	minObsDur := in.Config.Policy.MinObservationWindow.Duration()
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

			protectedCount++
			if fu.TransitivelyLive && !fu.HasDirectLiveUsage() {
				report.ProtectedReasons[famName] = fmt.Sprintf("transitively live via recording rule(s): %v", fu.TransitiveLiveVia)
			} else {
				report.ProtectedReasons[famName] = "actively consumed"
			}
			continue
		}

		// Unused family: check observation window (Invariant #5)
		obsDays := math.Round(f.ObservedDuration.Hours()/24.0*10) / 10
		if f.ObservedDuration < minObsDur {
			protectedCount++
			report.ProtectedReasons[famName] = fmt.Sprintf("observed %.1fd < minObservationWindow %.0fd", obsDays, requiredDays)
			continue
		}

		// Eligible for DROP_FAMILY!
		reason := fmt.Sprintf("0 live consumers (observed %.0fd)", obsDays)
		if deadRules := dagRes.SourceDeadChains[famName]; len(deadRules) > 0 {
			reason = fmt.Sprintf("dead recording-rule chain (%d rules)", len(deadRules))
		}
		rec := metrics.Recommendation{
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
		}
		if _, already := ledger.ActiveDrops[famName]; already {
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
		existingActiveDrops = append(existingActiveDrops, metrics.Recommendation{
			FamilyName:    famName,
			MetricType:    metrics.MetricTypeUnknown,
			Action:        dropRec.Action,
			Confidence:    report.Summary.Confidence,
			Series:        dropRec.SeriesAtDrop,
			Reason:        dropRec.Reason,
			TrancheNumber: dropRec.Tranche,
			Synthesized:   true,
			Members:       []string{famName},
			NamingMode:    metrics.NamingMode(in.Config.Inventory.NamingMode),
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
		report.ProjectedDrops = allDroppable
		report.Summary.SynthesizedFamilies = 0
		report.Summary.SynthesizedSeries = 0
		report.Summary.SynthesizedPercent = 0
		if len(plannedTranches) > 0 {
			report.Summary.ActiveTranche = 0
		}
	} else {
		// Determine whether we can unlock the next tranche (soak complete + 0 unresolved regrets).
		canUnlockNext := true
		if lastT := ledger.LatestTranche(); lastT != nil {
			if lastT.UnresolvedRegrets > 0 {
				canUnlockNext = false
				report.Safety.Warnings = append(report.Safety.Warnings, fmt.Sprintf(
					"tranche %d has %d unresolved regret(s); holding next tranche rollout",
					lastT.Number, lastT.UnresolvedRegrets,
				))
			} else if now.Before(lastT.SoakUntil) {
				canUnlockNext = false
				report.Safety.SoakBlocked = true
				report.Safety.SoakRemaining = lastT.SoakUntil.Sub(now)
			} else {
				lastT.Status = "soaked"
			}
		}

		activeDrops := append([]metrics.Recommendation(nil), existingActiveDrops...)
		activeTrancheNum := priorTranches
		if canUnlockNext && len(plannedTranches) > 0 {
			nextTranche := plannedTranches[0]
			activeTrancheNum = nextTranche.Number
			for i := range nextTranche.Items {
				nextTranche.Items[i].Synthesized = true
				activeDrops = append(activeDrops, nextTranche.Items[i])
			}
			ledger.RecordTrancheActivation(nextTranche.Number, now, in.Config.Policy.SoakWindow.Duration(), nextTranche.Items)
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
	if in.QueryLog != nil && in.QueryLog.Configured {
		sc.QueryLogEntries = state.IntPtr(in.QueryLog.TotalEntries)
	}
	return sc
}

func detectWildcardsAndExclusions(cat *FamilyCatalog, refs []metrics.UsageRef, policy config.PolicyConfig) ([]metrics.WildcardSource, map[string]bool) {
	totalFamilies := len(cat.OrderedNames)
	excludedSources := make(map[string]bool)
	if totalFamilies == 0 {
		return nil, excludedSources
	}

	type sourceAgg struct {
		sourceID   string
		sourceName string
		origin     metrics.UsageOrigin
		matched    map[string]struct{}
		excluded   bool
	}
	bySource := make(map[string]*sourceAgg)

	for _, ref := range refs {
		if ref.Origin == metrics.OriginRuleEval {
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
			wildcards = append(wildcards, metrics.WildcardSource{
				SourceID:        sa.sourceID,
				SourceName:      sa.sourceName,
				Origin:          sa.origin,
				MatchedFamilies: len(sa.matched),
				TotalFamilies:   totalFamilies,
				MatchPercent:    math.Round(pct*10) / 10,
				Excluded:        sa.excluded,
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
		if fu == nil || !fu.IsLive() || len(f.Stats.Labels) == 0 || f.Stats.TotalSeries <= 0 {
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
