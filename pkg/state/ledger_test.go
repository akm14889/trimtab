package state

import (
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
	l.RecordSeenFamily("foo_metric", now.Add(-40*24*time.Hour), now)
	l.UpdateQueryLogCoverage(now.Add(-30*24*time.Hour), now, 500, 100, 10)
	l.RecordTrancheActivation(1, now.Add(-24*time.Hour), 7*24*time.Hour, []metrics.Recommendation{
		{FamilyName: "foo_metric", Action: metrics.ActionDropFamily, Series: 1000},
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
	if loaded.QueryLogCoverage.CoveredDays < 29.9 {
		t.Fatalf("QueryLogCoverage.CoveredDays = %v, want ~30", loaded.QueryLogCoverage.CoveredDays)
	}
	if _, ok := loaded.ActiveDrops["foo_metric"]; !ok {
		t.Fatal("foo_metric should be in ActiveDrops")
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
