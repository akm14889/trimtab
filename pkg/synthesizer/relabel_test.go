package synthesizer

import (
	"fmt"
	"reflect"
	"testing"

	"trimtab.io/trimtab/pkg/metrics"
)

func TestBuildRelabelRegexTerm(t *testing.T) {
	tests := []struct {
		name string
		rec  metrics.Recommendation
		want string
	}{
		{
			name: "gauge family",
			rec: metrics.Recommendation{
				FamilyName: "checkout_legacy_cart_gauge",
				MetricType: metrics.MetricTypeGauge,
				Action:     metrics.ActionDropFamily,
			},
			want: "checkout_legacy_cart_gauge",
		},
		{
			name: "counter with _created member",
			rec: metrics.Recommendation{
				FamilyName: "req_total",
				MetricType: metrics.MetricTypeCounter,
				Action:     metrics.ActionDropFamily,
				Members:    []string{"req_total", "req_created"},
			},
			want: "req_created|req_total",
		},
		{
			name: "classic histogram family drop",
			rec: metrics.Recommendation{
				FamilyName: "orders_deprecated_latency_seconds",
				MetricType: metrics.MetricTypeHistogram,
				Action:     metrics.ActionDropFamily,
			},
			want: "orders_deprecated_latency_seconds_(bucket|sum|count|created)",
		},
		{
			name: "classic histogram bucket-only drop",
			rec: metrics.Recommendation{
				FamilyName: "api_request_duration_seconds",
				MetricType: metrics.MetricTypeHistogram,
				Action:     metrics.ActionDropHistogramBuckets,
			},
			want: "api_request_duration_seconds_bucket",
		},
		{
			name: "native histogram family drop",
			rec: metrics.Recommendation{
				FamilyName: "native_rpc_duration_seconds",
				MetricType: metrics.MetricTypeNativeHistogram,
				Action:     metrics.ActionDropFamily,
			},
			want: "native_rpc_duration_seconds",
		},
		{
			name: "summary family drop",
			rec: metrics.Recommendation{
				FamilyName: "gc_pause_seconds",
				MetricType: metrics.MetricTypeSummary,
				Action:     metrics.ActionDropFamily,
			},
			want: "gc_pause_seconds(|_(sum|count|created))",
		},
		{
			name: "metric name with regex metacharacters",
			rec: metrics.Recommendation{
				FamilyName: "http.server.duration",
				MetricType: metrics.MetricTypeGauge,
				Action:     metrics.ActionDropFamily,
			},
			want: `http\.server\.duration`,
		},
		{
			name: "ignored action keep",
			rec: metrics.Recommendation{
				FamilyName: "up",
				MetricType: metrics.MetricTypeGauge,
				Action:     metrics.ActionKeep,
			},
			want: "",
		},
		{
			name: "ignored action drop dimension advisory",
			rec: metrics.Recommendation{
				FamilyName: "http_requests_total",
				MetricType: metrics.MetricTypeCounter,
				Action:     metrics.ActionDropDimensionAdvisory,
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildRelabelRegexTerm(tc.rec)
			if got != tc.want {
				t.Fatalf("BuildRelabelRegexTerm(%+v) = %q, want %q", tc.rec, got, tc.want)
			}
		})
	}
}

func TestChunkRelabelRegexes(t *testing.T) {
	if got := ChunkRelabelRegexes(nil, 40); got != nil {
		t.Fatalf("expected nil for empty terms, got %v", got)
	}
	if got := ChunkRelabelRegexes([]string{"a"}, 40); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("single term mismatch: %v", got)
	}

	makeTerms := func(n int) []string {
		out := make([]string, n)
		for i := range n {
			out[i] = fmt.Sprintf("m_%03d", i)
		}
		return out
	}

	if got := ChunkRelabelRegexes(makeTerms(40), 40); len(got) != 1 {
		t.Fatalf("40 terms with cap 40 should yield 1 chunk, got %d", len(got))
	}
	if got := ChunkRelabelRegexes(makeTerms(41), 40); len(got) != 2 {
		t.Fatalf("41 terms with cap 40 should yield 2 chunks, got %d", len(got))
	}
	chunks85 := ChunkRelabelRegexes(makeTerms(85), 0) // defaults to DefaultMaxTermsPerRule (40)
	if len(chunks85) != 3 {
		t.Fatalf("85 terms with default cap should yield 3 chunks, got %d", len(chunks85))
	}
}

func TestSortActiveDrops_DoesNotMutateInput(t *testing.T) {
	in := []metrics.Recommendation{
		{FamilyName: "b_small", Series: 10},
		{FamilyName: "a_large", Series: 500},
		{FamilyName: "a_tie", Series: 100},
		{FamilyName: "b_tie", Series: 100},
	}
	orig := append([]metrics.Recommendation(nil), in...)

	sorted := SortActiveDrops(in)
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("SortActiveDrops mutated input slice")
	}

	wantOrder := []string{"a_large", "a_tie", "b_tie", "b_small"}
	for i, want := range wantOrder {
		if sorted[i].FamilyName != want {
			t.Fatalf("sorted[%d] = %q, want %q", i, sorted[i].FamilyName, want)
		}
	}
}
