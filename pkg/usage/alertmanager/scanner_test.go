package alertmanager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"trimtab.io/trimtab/pkg/config"
)

func TestScanAlertmanagerYAMLAndTemplates(t *testing.T) {
	const amYAML = `
route:
  receiver: default-slack
  group_by: ['alertname', 'cluster']
  matchers:
    - severity="critical"
  routes:
    - receiver: pagerduty-payments
      group_by: [service]
      match:
        team: payments
      match_re:
        env: "prod|staging"
      matchers:
        - 'region =~ "us-.*"'
inhibit_rules:
  - source_match:
      severity: critical
    target_matchers:
      - 'inhibition_target="true"'
    equal: ['namespace', 'pod']
receivers:
  - name: pagerduty-payments
    slack_configs:
      - title: '[{{ .GroupLabels.severity }}] {{ .CommonLabels.service }} ({{ index .CommonLabels "owner" }})'
        text: '{{ range .CommonLabels.SortedPairs }}{{ .Name }}={{ .Value }}{{ end }} on {{ .GroupLabels["tier"] }}'
`
	dir := t.TempDir()
	path := filepath.Join(dir, "alertmanager.yaml")
	if err := os.WriteFile(path, []byte(amYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(context.Background(), &config.AlertmanagerConfig{
		ConfigPaths: []string{path},
	})
	if err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}
	if res.PreservesAllLabels {
		t.Errorf("did not expect PreservesAllLabels=true without group_by '...'")
	}
	wantLabels := []string{
		"alertname", "cluster", "env", "inhibition_target",
		"namespace", "owner", "pod", "region", "service",
		"severity", "team", "tier",
	}
	if !slices.Equal(res.ProtectedLabels, wantLabels) {
		t.Fatalf("ProtectedLabels = %v, want %v", res.ProtectedLabels, wantLabels)
	}
	if res.MatcherCount == 0 {
		t.Fatalf("expected non-zero MatcherCount for baseline drift tracking")
	}

	// Ensure group_by: ['...'] preserves all labels
	allRes, err := ParseYAML([]byte(`
route:
  receiver: catchall
  group_by: ['...']
`))
	if err != nil {
		t.Fatal(err)
	}
	if !allRes.PreservesAllLabels {
		t.Fatalf("expected group_by '...' to set PreservesAllLabels=true")
	}

	// Missing glob fails closed
	if _, err := Scan(context.Background(), &config.AlertmanagerConfig{ConfigPaths: []string{filepath.Join(dir, "missing-*.yaml")}}); err == nil {
		t.Fatal("expected missing configPaths glob to fail closed")
	}
}

func TestScanAlertmanagerHTTPAndCRD(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/status" || r.Header.Get("Authorization") != "Bearer am-tok" {
			http.Error(w, "denied am-tok", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{
  "cluster": {"status": "ready"},
  "config": {
    "original": "route:\n  receiver: ops\n  group_by: [routing_label]\n  matchers:\n    - 'escalation_tier=\"p1\"'\nreceivers:\n  - name: ops\n    webhook_configs:\n      - url: https://hooks.local\n"
  }
}`))
	}))
	defer srv.Close()

	res, err := Scan(context.Background(), &config.AlertmanagerConfig{
		URL:         srv.URL,
		BearerToken: "am-tok",
	})
	if err != nil {
		t.Fatalf("Scan(HTTP) failed: %v", err)
	}
	if !slices.Equal(res.ProtectedLabels, []string{"escalation_tier", "routing_label"}) {
		t.Fatalf("HTTP ProtectedLabels = %v, want [escalation_tier routing_label]", res.ProtectedLabels)
	}

	// Verify 401 fails closed with secret redacted
	_, err = Scan(context.Background(), &config.AlertmanagerConfig{
		URL:         srv.URL,
		BearerToken: "wrong-am-secret",
	})
	if err == nil {
		t.Fatal("expected HTTP 401 to fail closed")
	}
	if strings.Contains(err.Error(), "wrong-am-secret") {
		t.Fatalf("secret leaked in error: %v", err)
	}

	// Verify Prometheus Operator AlertmanagerConfig CRD parsing
	crdRes, err := ParseYAML([]byte(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: AlertmanagerConfig
metadata:
  name: team-a
spec:
  route:
    receiver: team-a-slack
    groupBy: [job, instance]
    matchers:
      - name: severity
        value: warning
        matchType: "="
  inhibitRules:
    - sourceMatch:
        - name: severity
          value: critical
      equal: [cluster]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cluster", "instance", "job", "severity"} {
		if !slices.Contains(crdRes.ProtectedLabels, want) {
			t.Errorf("CRD parse missing %q in %v", want, crdRes.ProtectedLabels)
		}
	}
}
