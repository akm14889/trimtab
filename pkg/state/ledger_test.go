package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/metrics"
)

func TestLedgerPersistenceAndRegret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", ".trimtab-state.json")

	// Missing file returns fresh ledger
	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing file error: %v", err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	l.UpdatedAt = now
	l.BaselineCounts.Dashboards = IntPtr(100)
	l.BaselineCounts.QueryLogDailyRate = Float64Ptr(120.5)
	l.RecordSeenFamily("foo_metric", now.Add(-40*24*time.Hour), now)
	l.RecordLastBucketQueried("hist_metric", now.Add(-5*24*time.Hour))

	observed := make(map[string]int)
	for i := 0; i < 30; i++ {
		d := now.Add(-time.Duration(i) * 24 * time.Hour).UTC().Format("2006-01-02")
		observed[d] = 10
	}
	l.UpdateQueryLogCoverage(observed, now.Add(-29*24*time.Hour), now, 500, 100, 10, now, 30*24*time.Hour)
	l.RecordTrancheActivation(1, now.Add(-24*time.Hour), 7*24*time.Hour, false, []metrics.Recommendation{
		{
			FamilyName: "foo_metric",
			MetricType: metrics.MetricTypeHistogram,
			Action:     metrics.ActionDropFamily,
			Series:     1000,
			Members:    []string{"foo_metric_bucket", "foo_metric_sum", "foo_metric_count"},
		},
	})

	if err := l.Save(path); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load saved ledger error: %v", err)
	}
	if loaded.BaselineCounts.Dashboards == nil || *loaded.BaselineCounts.Dashboards != 100 {
		t.Fatalf("BaselineCounts.Dashboards not persisted: %+v", loaded.BaselineCounts)
	}
	if loaded.BaselineCounts.QueryLogDailyRate == nil || *loaded.BaselineCounts.QueryLogDailyRate != 120.5 {
		t.Fatalf("BaselineCounts.QueryLogDailyRate not persisted: %+v", loaded.BaselineCounts)
	}
	if loaded.QueryLogCoverage.CoveredDays != 30 {
		t.Fatalf("QueryLogCoverage.CoveredDays = %v, want 30", loaded.QueryLogCoverage.CoveredDays)
	}
	if sf := loaded.SeenFamilies["hist_metric"]; sf == nil || sf.LastBucketQueriedAt.IsZero() {
		t.Fatalf("LastBucketQueriedAt not persisted: %+v", sf)
	}
	ad, ok := loaded.ActiveDrops["foo_metric"]
	if !ok {
		t.Fatal("foo_metric should be in ActiveDrops")
	}
	if len(ad.Members) != 3 || ad.MetricType != metrics.MetricTypeHistogram {
		t.Fatalf("ActiveDropRecord metadata not persisted: %+v", ad)
	}
	if loaded.LatestTranche().Status != "pending_confirmation" {
		t.Fatalf("expected pending_confirmation status, got %q", loaded.LatestTranche().Status)
	}
	if !loaded.ConfirmLatestTranche(now, 7*24*time.Hour) {
		t.Fatal("expected ConfirmLatestTranche to return true")
	}
	if loaded.LatestTranche().Status != "soaking" || loaded.LatestTranche().ConfirmedAt.IsZero() {
		t.Fatalf("expected soaking status and ConfirmedAt set, got %+v", loaded.LatestTranche())
	}

	// Trigger regret
	loaded.RecordRegret(metrics.RegretEvent{
		FamilyName: "foo_metric",
		DetectedAt: now,
		Query:      "foo_metric",
		HitCount:   1,
	})
	if _, ok := loaded.ActiveDrops["foo_metric"]; ok {
		t.Fatal("foo_metric should have been removed from ActiveDrops on regret")
	}
	if !loaded.IsPinnedNeverDrop("foo_metric") {
		t.Fatal("foo_metric should be pinned to neverDrop on regret")
	}
	if loaded.LatestTranche().Status != "blocked_regret" {
		t.Fatalf("LatestTranche status = %q, want blocked_regret", loaded.LatestTranche().Status)
	}
}

func TestLoadV1LedgerBackwardCompatAndTrailingBitmap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v1-state.json")
	v1JSON := `{
  "version": 1,
  "updated_at": "2026-11-20T00:00:00Z",
  "baseline_counts": {
    "dashboards": 42,
    "query_log_entries": 1000
  },
  "query_log_coverage": {
    "earliest_timestamp": "2026-10-20T00:00:00Z",
    "latest_timestamp": "2026-11-20T00:00:00Z",
    "covered_days": 31
  },
  "seen_families": {
    "legacy_metric": {
      "first_seen_at": "2026-10-01T00:00:00Z",
      "last_seen_at": "2026-11-20T00:00:00Z",
      "last_queried_at": "2026-11-05T00:00:00Z"
    }
  },
  "tranches": [
    {
      "number": 1,
      "started_at": "2026-11-18T00:00:00Z",
      "soak_until": "2026-11-25T00:00:00Z",
      "status": "active",
      "families": ["unused_m"],
      "series": 100
    }
  ],
  "active_drops": {
    "unused_m": {
      "family_name": "unused_m",
      "action": "DROP_FAMILY",
      "dropped_at": "2026-11-18T00:00:00Z",
      "tranche": 1,
      "series_at_drop": 100
    }
  }
}`
	if err := os.WriteFile(path, []byte(v1JSON), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path)
	if err != nil {
		t.Fatalf("Load v1 ledger error: %v", err)
	}
	if l.QueryLogCoverage.CoveredDates == nil {
		t.Fatal("CoveredDates map must be initialized on v1 load")
	}
	if l.BaselineCounts.QueryLogDailyRate != nil {
		t.Fatalf("expected nil QueryLogDailyRate on v1 load, got %v", l.BaselineCounts.QueryLogDailyRate)
	}

	// Trailing window test: 5 early days + 5 recent days across 30d span = 10 covered days (not 30!).
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	l2 := NewLedger()
	dates := make(map[string]int)
	for i := 0; i < 5; i++ {
		dates[now.Add(-time.Duration(29-i)*24*time.Hour).Format("2006-01-02")] = 5
		dates[now.Add(-time.Duration(i)*24*time.Hour).Format("2006-01-02")] = 5
	}
	l2.UpdateQueryLogCoverage(dates, now.Add(-29*24*time.Hour), now, 50, 0, 0, now, 30*24*time.Hour)
	if l2.QueryLogCoverage.CoveredDays != 10 {
		t.Fatalf("CoveredDays = %v, want 10 for gapped trailing window", l2.QueryLogCoverage.CoveredDays)
	}
}
