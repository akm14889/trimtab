package engine

import (
	"fmt"
	"sort"

	"trimtab.io/trimtab/pkg/metrics"
	"trimtab.io/trimtab/pkg/usage/promrules"
)

// DAGResult captures the outcome of recording-rule transitive closure analysis (Invariant #8).
type DAGResult struct {
	DeadRules          []metrics.DeadRuleAdvisory
	DeadRecordedNames  map[string]promrules.RecordingRule
	LiveRecordedNames  map[string]bool
	SourceDeadChains   map[string][]string // source family -> list of dead recording rule names consuming it
}

// ResolveRecordingRuleDAG builds a directed dependency graph from recording rules
// (`source -> R1 -> R2`) and walks backwards from all live sinks (`api-user` query logs,
// non-excluded Grafana dashboards, Grafana alerts, Prometheus alerting rules).
//
// If R2 has a live sink -> R1 and source are transitively LIVE (and R1/R2 label usage merges in).
// If neither R2 nor R1 has any live sink -> R1 & R2 are emitted as DeadRuleAdvisories and
// do NOT protect source from being dropped at the collector (Invariant #8).
func ResolveRecordingRuleDAG(cat *FamilyCatalog, usage *metrics.UsageIndex, rules []promrules.RecordingRule) *DAGResult {
	res := &DAGResult{
		DeadRecordedNames: make(map[string]promrules.RecordingRule),
		LiveRecordedNames: make(map[string]bool),
		SourceDeadChains:  make(map[string][]string),
	}
	if len(rules) == 0 {
		return res
	}

	// Index recording rules by recorded metric name (multiple rules may share a record name).
	rulesByName := make(map[string][]promrules.RecordingRule)
	for _, r := range rules {
		rulesByName[r.Name] = append(rulesByName[r.Name], r)
	}

	// Resolve each recording rule's input dependencies (either catalog families or other recording rules).
	type ruleDep struct {
		targetName string
		role       string
		explicit   []string
		preserves  bool
		without    []string
	}
	resolveRuleDeps := func(r promrules.RecordingRule) []ruleDep {
		var deps []ruleDep
		for _, acc := range r.Inputs {
			if !acc.IsRegex {
				if matches := cat.MatchExactName(acc.MetricName); len(matches) > 0 {
					for _, m := range matches {
						deps = append(deps, ruleDep{
							targetName: m.FamilyName,
							role:       m.Role,
							explicit:   mergeLabelSlices(acc.ExplicitLabels, r.TemplateLabels),
							preserves:  acc.PreservesAll,
							without:    acc.WithoutLabels,
						})
					}
				} else if acc.MetricName != "" {
					// Input may be an upstream recording rule not currently in inventory
					deps = append(deps, ruleDep{
						targetName: acc.MetricName,
						role:       "base",
						explicit:   mergeLabelSlices(acc.ExplicitLabels, r.TemplateLabels),
						preserves:  acc.PreservesAll,
						without:    acc.WithoutLabels,
					})
				}
			} else {
				for _, m := range cat.MatchRegex(acc.MetricRegex) {
					deps = append(deps, ruleDep{
						targetName: m.FamilyName,
						role:       m.Role,
						explicit:   mergeLabelSlices(acc.ExplicitLabels, r.TemplateLabels),
						preserves:  acc.PreservesAll,
						without:    acc.WithoutLabels,
					})
				}
			}
		}
		return deps
	}

	// Seed BFS queue with every node that has direct live usage (sinks or ledger-retained recent queries).
	liveNodes := make(map[string]bool)
	queue := make([]string, 0)
	for famName, fu := range usage.Families {
		if fu.HasDirectLiveUsage() || fu.LiveFromLedger {
			if !liveNodes[famName] {
				liveNodes[famName] = true
				queue = append(queue, famName)
			}
		}
	}
	// Also seed any recorded rule name directly referenced by live usage even if not in inventory.
	for recName := range rulesByName {
		if fu, ok := usage.Families[recName]; ok && (fu.HasDirectLiveUsage() || fu.LiveFromLedger) {
			if !liveNodes[recName] {
				liveNodes[recName] = true
				queue = append(queue, recName)
			}
		}
	}

	// Walk backwards: live recorded metric -> its upstream inputs are transitively LIVE.
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		recRules, isRecorded := rulesByName[curr]
		if !isRecorded {
			continue
		}
		res.LiveRecordedNames[curr] = true

		for _, r := range recRules {
			for _, dep := range resolveRuleDeps(r) {
				fu := usage.GetOrCreate(dep.targetName)
				fu.TransitivelyLive = true
				fu.TransitiveLiveVia = appendUnique(fu.TransitiveLiveVia, r.Name)
				applyRoleUsage(fu, dep.role)
				if dep.preserves {
					fu.Labels.MarkPreservesAll()
				} else if len(dep.without) > 0 {
					fu.Labels.MarkWithout(dep.without)
				}
				fu.Labels.MarkExplicit(dep.explicit...)

				if !liveNodes[dep.targetName] {
					liveNodes[dep.targetName] = true
					queue = append(queue, dep.targetName)
				}
			}
		}
	}

	// Collect all DEAD recording rules and trace them back to raw source families.
	deadRuleMap := make(map[string][]promrules.RecordingRule)
	for recName, recRules := range rulesByName {
		if liveNodes[recName] {
			continue
		}
		deadRuleMap[recName] = recRules
		for _, r := range recRules {
			res.DeadRecordedNames[r.Name] = r
		}
	}

	// Helper to find all raw (non-recording) source families feeding a dead recording rule.
	findSourceFamilies := func(startRule string) []string {
		visited := map[string]bool{startRule: true}
		q := []string{startRule}
		sources := make(map[string]struct{})
		for len(q) > 0 {
			curr := q[0]
			q = q[1:]
			for _, r := range rulesByName[curr] {
				for _, dep := range resolveRuleDeps(r) {
					if _, isRec := rulesByName[dep.targetName]; isRec {
						if !visited[dep.targetName] {
							visited[dep.targetName] = true
							q = append(q, dep.targetName)
						}
					} else {
						sources[dep.targetName] = struct{}{}
					}
				}
			}
		}
		out := make([]string, 0, len(sources))
		for s := range sources {
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}

	for _, r := range rules {
		if liveNodes[r.Name] {
			continue
		}
		srcs := findSourceFamilies(r.Name)
		note := "ruler bypasses collector — delete rule from rule files"
		if r.HasOrVectorZero {
			note = "uses `or vector(0)` and will keep emitting 0-value series until deleted from rule files"
		}
		res.DeadRules = append(res.DeadRules, metrics.DeadRuleAdvisory{
			RuleName:        r.Name,
			File:            r.File,
			Group:           r.Group,
			Expr:            r.Expr,
			SourceFamilies:  srcs,
			HasOrVectorZero: r.HasOrVectorZero,
			Note:            note,
		})
		for _, src := range srcs {
			res.SourceDeadChains[src] = appendUnique(res.SourceDeadChains[src], r.Name)
			fu := usage.GetOrCreate(src)
			fu.DeadRecordingChain = appendUnique(fu.DeadRecordingChain, r.Name)
		}
	}

	sort.Slice(res.DeadRules, func(i, j int) bool {
		if res.DeadRules[i].RuleName != res.DeadRules[j].RuleName {
			return res.DeadRules[i].RuleName < res.DeadRules[j].RuleName
		}
		return fmt.Sprintf("%s:%s", res.DeadRules[i].File, res.DeadRules[i].Group) <
			fmt.Sprintf("%s:%s", res.DeadRules[j].File, res.DeadRules[j].Group)
	})

	return res
}

func applyRoleUsage(fu *metrics.FamilyUsage, role string) {
	switch role {
	case "bucket":
		fu.UsedBuckets = true
	case "sum", "count", "created":
		fu.UsedSumOrCount = true
	default:
		fu.UsedBaseMetric = true
	}
}

func mergeLabelSlices(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func appendUnique(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}
