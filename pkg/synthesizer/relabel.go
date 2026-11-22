package synthesizer

import (
	"regexp"
	"sort"
	"strings"

	"trimtab.io/trimtab/pkg/metrics"
)

// DefaultMaxTermsPerRule bounds regex alternation terms per relabel rule to keep
// generated Prometheus/Alloy configs readable and well under regex size limits.
const DefaultMaxTermsPerRule = 40

// SortActiveDrops returns a deterministically ordered copy of drops
// (descending by Series, then ascending by FamilyName).
func SortActiveDrops(drops []metrics.Recommendation) []metrics.Recommendation {
	out := append([]metrics.Recommendation(nil), drops...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Series != out[j].Series {
			return out[i].Series > out[j].Series
		}
		return out[i].FamilyName < out[j].FamilyName
	})
	return out
}

// BuildRelabelRegexTerm returns an implicitly anchored Prometheus relabel regex term
// (no leading ^ or trailing $) for a single drop Recommendation.
func BuildRelabelRegexTerm(rec metrics.Recommendation) string {
	base := regexp.QuoteMeta(rec.FamilyName)
	switch rec.Action {
	case metrics.ActionDropHistogramBuckets:
		return base + "_bucket"
	case metrics.ActionDropFamily:
		switch rec.MetricType {
		case metrics.MetricTypeHistogram:
			return base + "_(bucket|sum|count|created)"
		case metrics.MetricTypeSummary:
			return base + "(|_(sum|count|created))"
		default:
			if len(rec.Members) > 1 {
				escaped := make([]string, len(rec.Members))
				for i, m := range rec.Members {
					escaped[i] = regexp.QuoteMeta(m)
				}
				sort.Strings(escaped)
				return strings.Join(escaped, "|")
			}
			return base
		}
	default:
		return ""
	}
}

// CollectRelabelTerms sorts active drops, skips AdvisoryOnly entries, builds regex terms,
// and returns the terms alongside total dropped family and series counts.
func CollectRelabelTerms(drops []metrics.Recommendation) (terms []string, droppedFamilies, droppedSeries int) {
	sorted := SortActiveDrops(drops)
	for _, rec := range sorted {
		if rec.AdvisoryOnly {
			continue
		}
		term := BuildRelabelRegexTerm(rec)
		if term == "" {
			continue
		}
		terms = append(terms, term)
		droppedFamilies++
		droppedSeries += rec.Series
	}
	return terms, droppedFamilies, droppedSeries
}

// ChunkRelabelRegexes splits regex alternation terms into bounded chunks joined by "|".
// If maxTermsPerRule <= 0, DefaultMaxTermsPerRule is used.
func ChunkRelabelRegexes(terms []string, maxTermsPerRule int) []string {
	if len(terms) == 0 {
		return nil
	}
	if maxTermsPerRule <= 0 {
		maxTermsPerRule = DefaultMaxTermsPerRule
	}
	chunks := make([]string, 0, (len(terms)+maxTermsPerRule-1)/maxTermsPerRule)
	for i := 0; i < len(terms); i += maxTermsPerRule {
		end := i + maxTermsPerRule
		if end > len(terms) {
			end = len(terms)
		}
		chunks = append(chunks, strings.Join(terms[i:end], "|"))
	}
	return chunks
}
