package engine

import (
	"regexp"
	"sort"
	"strings"

	"trimtab.io/trimtab/pkg/inventory/prometheus"
	"trimtab.io/trimtab/pkg/metrics"
)

// FamilyCatalog indexes all metric families and maps individual Prometheus series names back to families.
type FamilyCatalog struct {
	Families       map[string]*metrics.MetricFamily
	OrderedNames   []string
	MemberToFamily map[string]string
	MemberRole     map[string]string // "base", "bucket", "sum", "count", "created"
}

// GroupFamilies groups raw Prometheus metric names into MetricFamilies so classic histograms
// (_bucket/_sum/_count/_created) and summaries are never orphaned, while recognizing native
// histograms separately (Invariant #7).
func GroupFamilies(snap *prometheus.InventorySnapshot) *FamilyCatalog {
	cat := &FamilyCatalog{
		Families:       make(map[string]*metrics.MetricFamily),
		MemberToFamily: make(map[string]string),
		MemberRole:     make(map[string]string),
	}
	if snap == nil {
		return cat
	}

	nameSet := make(map[string]struct{}, len(snap.MetricNames))
	for _, n := range snap.MetricNames {
		nameSet[n] = struct{}{}
	}
	for n := range snap.SeriesCounts {
		nameSet[n] = struct{}{}
	}

	allNames := make([]string, 0, len(nameSet))
	for n := range nameSet {
		allNames = append(allNames, n)
	}
	sort.Strings(allNames)

	getOrCreate := func(base string, typ metrics.MetricType, help string, native bool) *metrics.MetricFamily {
		f, ok := cat.Families[base]
		if !ok {
			nm := snap.NamingMode
			if nm == "" {
				nm = metrics.NamingPrometheusReceiver
			}
			otlpName := ""
			if snap.OTLPNameMap != nil {
				if mapped, exists := snap.OTLPNameMap[base]; exists && mapped != "" {
					otlpName = mapped
					nm = metrics.NamingExplicitOTLP
				}
			}
			f = &metrics.MetricFamily{
				Name:              base,
				Type:              typ,
				Help:              help,
				IsNativeHistogram: native,
				NamingMode:        nm,
				OTLPName:          otlpName,
				Stats: metrics.SeriesStats{
					MemberSeries: make(map[string]int),
				},
			}
			cat.Families[base] = f
		} else {
			if f.Type == metrics.MetricTypeUnknown && typ != metrics.MetricTypeUnknown {
				f.Type = typ
			}
			if f.Help == "" && help != "" {
				f.Help = help
			}
			if native {
				f.IsNativeHistogram = true
				f.Type = metrics.MetricTypeNativeHistogram
			}
		}
		return f
	}

	// Pass 1: identify families from metadata and histogram/summary suffix structures.
	for _, rawName := range allNames {
		// Native histogram (Prometheus 3 / OTel exponential histogram) — single-series instrument.
		if snap.NativeHistograms[rawName] {
			meta := snap.Metadata[rawName]
			f := getOrCreate(rawName, metrics.MetricTypeNativeHistogram, meta.Help, true)
			addMember(cat, f, rawName, "base", snap)
			continue
		}

		base, role, typ, help := classifyMetricName(rawName, nameSet, snap.Metadata)
		f := getOrCreate(base, typ, help, false)
		addMember(cat, f, rawName, role, snap)
	}

	// Finalize sorted members, labels, and ordered family list.
	cat.OrderedNames = make([]string, 0, len(cat.Families))
	for name, f := range cat.Families {
		sort.Strings(f.Members)
		sort.Strings(f.Stats.Labels)
		cat.OrderedNames = append(cat.OrderedNames, name)
	}
	sort.Strings(cat.OrderedNames)
	return cat
}

func classifyMetricName(raw string, allNames map[string]struct{}, meta map[string]prometheus.MetricMetadata) (base, role string, typ metrics.MetricType, help string) {
	// Check classic histogram suffixes first.
	for _, suffix := range []struct {
		ext  string
		role string
	}{
		{"_bucket", "bucket"},
		{"_sum", "sum"},
		{"_count", "count"},
		{"_created", "created"},
	} {
		if strings.HasSuffix(raw, suffix.ext) && len(raw) > len(suffix.ext) {
			candidate := strings.TrimSuffix(raw, suffix.ext)
			if m, ok := meta[candidate]; ok {
				mt := normalizeMetricType(m.Type)
				if mt == metrics.MetricTypeHistogram || mt == metrics.MetricTypeSummary {
					return candidate, suffix.role, mt, m.Help
				}
				if suffix.ext == "_created" {
					return candidate, "created", mt, m.Help
				}
			}
			// Heuristic fallback when /api/v1/metadata is partial or missing:
			_, hasBucket := allNames[candidate+"_bucket"]
			_, hasSum := allNames[candidate+"_sum"]
			_, hasCount := allNames[candidate+"_count"]
			_, hasBase := allNames[candidate]
			if hasBucket && (hasSum || hasCount || suffix.ext == "_bucket") {
				return candidate, suffix.role, metrics.MetricTypeHistogram, meta[candidate].Help
			}
			if !hasBucket && hasSum && hasCount && (hasBase || suffix.ext == "_sum" || suffix.ext == "_count") {
				return candidate, suffix.role, metrics.MetricTypeSummary, meta[candidate].Help
			}
			if suffix.ext == "_created" {
				if hasBase {
					return candidate, "created", normalizeMetricType(meta[candidate].Type), meta[candidate].Help
				}
				if _, hasTotal := allNames[candidate+"_total"]; hasTotal {
					return candidate + "_total", "created", metrics.MetricTypeCounter, meta[candidate+"_total"].Help
				}
			}
		}
	}

	// Base metric name
	if m, ok := meta[raw]; ok {
		return raw, "base", normalizeMetricType(m.Type), m.Help
	}
	// Summary base check without metadata
	if _, hasSum := allNames[raw+"_sum"]; hasSum {
		if _, hasCount := allNames[raw+"_count"]; hasCount {
			if _, hasBucket := allNames[raw+"_bucket"]; !hasBucket {
				return raw, "base", metrics.MetricTypeSummary, ""
			}
		}
	}
	if strings.HasSuffix(raw, "_total") {
		return raw, "base", metrics.MetricTypeCounter, ""
	}
	return raw, "base", metrics.MetricTypeGauge, ""
}

func normalizeMetricType(t string) metrics.MetricType {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "counter":
		return metrics.MetricTypeCounter
	case "gauge":
		return metrics.MetricTypeGauge
	case "histogram":
		return metrics.MetricTypeHistogram
	case "native_histogram", "exponential_histogram":
		return metrics.MetricTypeNativeHistogram
	case "summary":
		return metrics.MetricTypeSummary
	case "info":
		return metrics.MetricTypeInfo
	case "stateset":
		return metrics.MetricTypeStateset
	default:
		return metrics.MetricTypeUnknown
	}
}

func addMember(cat *FamilyCatalog, f *metrics.MetricFamily, rawName, role string, snap *prometheus.InventorySnapshot) {
	cat.MemberToFamily[rawName] = f.Name
	cat.MemberRole[rawName] = role

	exists := false
	for _, m := range f.Members {
		if m == rawName {
			exists = true
			break
		}
	}
	if !exists {
		f.Members = append(f.Members, rawName)
	}

	count := snap.SeriesCounts[rawName]
	f.Stats.MemberSeries[rawName] = count
	f.Stats.TotalSeries += count
	switch role {
	case "bucket":
		f.Stats.BucketSeries += count
	case "sum", "count", "created":
		f.Stats.SumCountSeries += count
	}

	// Merge known labels
	if lbls := snap.MetricLabels[rawName]; len(lbls) > 0 {
		existing := make(map[string]struct{}, len(f.Stats.Labels))
		for _, l := range f.Stats.Labels {
			existing[l] = struct{}{}
		}
		for _, l := range lbls {
			if _, ok := existing[l]; !ok {
				existing[l] = struct{}{}
				f.Stats.Labels = append(f.Stats.Labels, l)
			}
		}
	}
	if sets := snap.SampleLabelSets[rawName]; len(sets) > 0 && len(f.Stats.SampleLabelSets) == 0 {
		f.Stats.SampleLabelSets = sets
	}

	// Propagate earliest first-seen hint across members
	if hint, ok := snap.FirstSeenHints[rawName]; ok && !hint.IsZero() {
		if f.FirstSeenAt.IsZero() || hint.Before(f.FirstSeenAt) {
			f.FirstSeenAt = hint
		}
	}
	if hint, ok := snap.FirstSeenHints[f.Name]; ok && !hint.IsZero() {
		if f.FirstSeenAt.IsZero() || hint.Before(f.FirstSeenAt) {
			f.FirstSeenAt = hint
		}
	}
}

// MatchRef returns all (familyName, role) pairs matched by a UsageRef (exact or regex).
func (cat *FamilyCatalog) MatchRef(ref metrics.UsageRef) []FamilyMatch {
	if !ref.IsRegex {
		return cat.MatchExactName(ref.MetricName)
	}
	return cat.MatchRegex(ref.MetricRegex)
}

type FamilyMatch struct {
	FamilyName string
	Role       string
}

func (cat *FamilyCatalog) MatchExactName(metricName string) []FamilyMatch {
	if cat == nil || metricName == "" {
		return nil
	}
	if fam, ok := cat.MemberToFamily[metricName]; ok {
		return []FamilyMatch{{FamilyName: fam, Role: cat.MemberRole[metricName]}}
	}
	if _, ok := cat.Families[metricName]; ok {
		return []FamilyMatch{{FamilyName: metricName, Role: "base"}}
	}
	// Heuristic fallback for metrics referenced in queries before inventory refresh
	for _, suf := range []struct{ ext, role string }{
		{"_bucket", "bucket"},
		{"_sum", "sum"},
		{"_count", "count"},
		{"_created", "created"},
	} {
		if strings.HasSuffix(metricName, suf.ext) {
			cand := strings.TrimSuffix(metricName, suf.ext)
			if _, ok := cat.Families[cand]; ok {
				return []FamilyMatch{{FamilyName: cand, Role: suf.role}}
			}
		}
	}
	return nil
}

func (cat *FamilyCatalog) MatchRegex(pattern string) []FamilyMatch {
	if cat == nil || pattern == "" {
		return nil
	}
	// Prometheus regex matchers are implicitly anchored: ^(?:pattern)$
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil
	}
	type key struct{ fam, role string }
	seen := make(map[key]struct{})
	var matches []FamilyMatch
	for member, fam := range cat.MemberToFamily {
		if re.MatchString(member) {
			role := cat.MemberRole[member]
			k := key{fam, role}
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				matches = append(matches, FamilyMatch{FamilyName: fam, Role: role})
			}
		}
	}
	// Also check family base names directly
	for _, fam := range cat.OrderedNames {
		if re.MatchString(fam) {
			k := key{fam, "base"}
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				matches = append(matches, FamilyMatch{FamilyName: fam, Role: "base"})
			}
		}
	}
	return matches
}
