package ruler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestScanMimirRulerYAML(t *testing.T) {
	const mimirYAML = `
checkout:
  - name: checkout_slo
    rules:
      - alert: CheckoutHighErrorRate
        expr: sum by (service) (rate(checkout_errors_total[5m])) / sum by (service) (rate(checkout_requests_total[5m])) > 0.05
        labels:
          severity: critical
        annotations:
          summary: "High errors on {{ $labels.cluster }}"
      - record: checkout:error_ratio:5m
        expr: rate(checkout_legacy_total[5m]) or vector(0)
infra:
  - name: infra_health
    rules:
      - record: node:cpu:avg5m
        expr: avg without (cpu) (rate(node_cpu_seconds_total[5m]))
`
	var gotTenant, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Scope-OrgID")
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/prometheus/config/v1/rules" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(mimirYAML))
	}))
	defer srv.Close()

	res, err := Scan(context.Background(), config.RulerConfig{
		Type:        "mimir",
		URL:         srv.URL,
		TenantID:    "prod-tenant",
		BearerToken: "secret-mimir-token",
	})
	if err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}
	if gotTenant != "prod-tenant" || gotAuth != "Bearer secret-mimir-token" {
		t.Fatalf("unexpected headers tenant=%q auth=%q", gotTenant, gotAuth)
	}
	if res.TotalRuleCount != 3 || res.AlertRuleCount != 1 || res.RecordingRuleCount != 2 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	if len(res.AlertRefs) != 2 {
		t.Fatalf("expected 2 alert refs, got %+v", res.AlertRefs)
	}
	for _, ref := range res.AlertRefs {
		if ref.Origin != metrics.OriginRulerAlert || !ref.Origin.IsLiveSink() {
			t.Fatalf("unexpected origin %q", ref.Origin)
		}
		hasService, hasCluster := false, false
		for _, lbl := range ref.ExplicitLabels {
			if lbl == "service" {
				hasService = true
			}
			if lbl == "cluster" {
				hasCluster = true
			}
		}
		if !hasService || !hasCluster {
			t.Errorf("expected both service (by) and cluster (template) labels on %s, got %v", ref.MetricName, ref.ExplicitLabels)
		}
	}

	if len(res.RecordingRules) != 2 {
		t.Fatalf("expected 2 recording rules, got %d", len(res.RecordingRules))
	}
	if res.RecordingRules[0].Name != "checkout:error_ratio:5m" || !res.RecordingRules[0].HasOrVectorZero {
		t.Fatalf("expected checkout:error_ratio:5m with HasOrVectorZero=true, got %+v", res.RecordingRules[0])
	}
}

func TestScanPrometheusThanosRulerJSON(t *testing.T) {
	const jsonResp = `{
  "status": "success",
  "data": {
    "groups": [
      {
        "name": "payments_alerts",
        "file": "/etc/thanos/rules/payments.yaml",
        "rules": [
          {
            "name": "PaymentLatencyHigh",
            "query": "histogram_quantile(0.99, sum by (le, service) (rate(payment_duration_seconds_bucket[5m]))) > 1",
            "type": "alerting",
            "labels": {"team": "payments"},
            "annotations": {"runbook": "https://runbooks/{{ $labels.region }}"}
          },
          {
            "name": "payments:duration:p99",
            "query": "histogram_quantile(0.99, sum by (le) (rate(payment_recorded_seconds_bucket[5m])))",
            "type": "recording"
          }
        ]
      }
    ]
  }
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/rules" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(jsonResp))
	}))
	defer srv.Close()

	res, err := ScanAll(context.Background(), []config.RulerConfig{
		{Type: "thanos", URL: srv.URL},
	})
	if err != nil {
		t.Fatalf("ScanAll() failed: %v", err)
	}
	if res.TotalRuleCount != 2 || res.AlertRuleCount != 1 || res.RecordingRuleCount != 1 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	if len(res.AlertRefs) != 1 || res.AlertRefs[0].MetricName != "payment_duration_seconds_bucket" {
		t.Fatalf("unexpected alert refs: %+v", res.AlertRefs)
	}
	if res.AlertRefs[0].SourceFolder != "/etc/thanos/rules/payments.yaml" {
		t.Errorf("unexpected sourceFolder: %q", res.AlertRefs[0].SourceFolder)
	}
	if len(res.RecordingRules) != 1 || res.RecordingRules[0].Name != "payments:duration:p99" {
		t.Fatalf("unexpected recording rules: %+v", res.RecordingRules)
	}
}

func TestScanRulerFailClosedAndRedact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied token super-secret-ruler-key"))
	}))
	defer srv.Close()

	_, err := Scan(context.Background(), config.RulerConfig{
		Type:        "mimir",
		URL:         srv.URL,
		BearerToken: "super-secret-ruler-key",
	})
	if err == nil {
		t.Fatal("expected HTTP 403 to fail closed")
	}
	if strings.Contains(err.Error(), "super-secret-ruler-key") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}
