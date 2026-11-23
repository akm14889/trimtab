package k8sconsumers

import (
	"os"
	"path/filepath"
	"testing"

	"trimtab.io/trimtab/pkg/config"
	"trimtab.io/trimtab/pkg/metrics"
)

func TestScanAllConsumerTypesAndList(t *testing.T) {
	const manifestsYAML = `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: api-scaler
  namespace: prod
spec:
  triggers:
    - type: prometheus
      name: http-queue
      metadata:
        serverAddress: http://prometheus:9090
        query: sum by (service) (rate(keda_http_queue_depth[2m]))
    - type: cpu
      metadata:
        value: "80"
---
apiVersion: keda.sh/v1alpha1
kind: ScaledJob
metadata:
  name: worker-scaler
  namespace: batch
spec:
  triggers:
    - type: prometheus
      metadata:
        query: max(batch_jobs_pending{queue="default"})
---
apiVersion: v1
kind: List
items:
  - apiVersion: argoproj.io/v1alpha1
    kind: AnalysisTemplate
    metadata:
      name: canary-success
      namespace: prod
    spec:
      args:
        - name: service-name
        - name: window
      metrics:
        - name: success-rate
          provider:
            prometheus:
              query: sum(rate(rollout_http_requests_total{app="{{args.service-name}}",status!~"5.."}[{{args.window}}])) / sum(rate(rollout_http_requests_total{app="{{args.service-name}}"}[5m]))
  - apiVersion: flagger.app/v1beta1
    kind: MetricTemplate
    metadata:
      name: request-duration
      namespace: istio-system
    spec:
      provider:
        type: prometheus
      query: |
        histogram_quantile(
          0.99,
          sum by (le, app) (
            rate(flagger_request_duration_seconds_bucket{namespace="{{ namespace }}",workload=~"{{ target }}"}[{{ interval }}])
          )
        )
  - apiVersion: flagger.app/v1beta1
    kind: MetricTemplate
    metadata:
      name: cloudwatch-ignored
      namespace: prod
    spec:
      provider:
        type: cloudwatch
      query: SELECT AVG(CPUUtilization)
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: adapter-config
  namespace: monitoring
data:
  config.yaml: |
    rules:
      - seriesQuery: '{__name__=~"^adapter_jobs_.*_total$",namespace!=""}'
        resources:
          overrides:
            namespace: {resource: "namespace"}
            pod: {resource: "pod"}
        metricsQuery: 'sum(rate(<<.Series>>{<<.LabelMatchers>>}[2m])) by (<<.GroupBy>>)'
    externalRules:
      - seriesQuery: 'sqs_visible_messages'
        resources:
          overrides:
            queue: {namespaceResource: "queues"}
        metricsQuery: 'sum(<<.Series>>{<<.LabelMatchers>>}) by (<<.GroupBy>>)'
    resourceRules:
      cpu:
        containerQuery: 'sum(rate(container_cpu_usage_seconds_total{<<.LabelMatchers>>}[3m])) by (<<.GroupBy>>)'
        resources:
          overrides:
            node: {resource: "node"}
            pod: {resource: "pod"}
        containerLabel: container
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: unrelated-cm
  namespace: default
data:
  foo.txt: hello world
`
	res, err := ScanSource(StaticSource{
		Manifests: []RawManifest{{Source: "bundle.yaml", Data: []byte(manifestsYAML)}},
	}, &config.K8sConsumersConfig{})
	if err != nil {
		t.Fatalf("ScanSource failed: %v", err)
	}
	if len(res.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if res.KEDATriggerCount != 2 {
		t.Errorf("KEDATriggerCount = %d, want 2", res.KEDATriggerCount)
	}
	if res.RolloutTemplateCount != 1 {
		t.Errorf("RolloutTemplateCount = %d, want 1", res.RolloutTemplateCount)
	}
	if res.FlaggerTemplateCount != 1 {
		t.Errorf("FlaggerTemplateCount = %d, want 1", res.FlaggerTemplateCount)
	}
	// 1 rules + 1 externalRules + 1 resourceRules.cpu.containerQuery = 3
	if res.PromAdapterRuleCount != 3 {
		t.Errorf("PromAdapterRuleCount = %d, want 3", res.PromAdapterRuleCount)
	}

	assertHasRef := func(origin metrics.UsageOrigin, metricName, metricRegex string, isRegex bool, expectedLabels ...string) {
		t.Helper()
		for _, ref := range res.Refs {
			if ref.Origin != origin || ref.IsRegex != isRegex {
				continue
			}
			if isRegex && ref.MetricRegex != metricRegex {
				continue
			}
			if !isRegex && ref.MetricName != metricName {
				continue
			}
			for _, wantLbl := range expectedLabels {
				found := false
				for _, gotLbl := range ref.ExplicitLabels {
					if gotLbl == wantLbl {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s/%s missing expected label %q in %v", origin, metricName+metricRegex, wantLbl, ref.ExplicitLabels)
				}
			}
			return
		}
		t.Errorf("missing expected ref origin=%s metric=%s regex=%s", origin, metricName, metricRegex)
	}

	assertHasRef(metrics.OriginKEDA, "keda_http_queue_depth", "", false, "service")
	assertHasRef(metrics.OriginKEDA, "batch_jobs_pending", "", false, "queue")
	assertHasRef(metrics.OriginArgoRollouts, "rollout_http_requests_total", "", false, "app", "status")
	assertHasRef(metrics.OriginFlagger, "flagger_request_duration_seconds_bucket", "", false, "le", "app", "namespace", "workload")
	assertHasRef(metrics.OriginPromAdapter, "", "^adapter_jobs_.*_total$", true, "namespace", "pod")
	assertHasRef(metrics.OriginPromAdapter, "sqs_visible_messages", "", false, "queue")
	assertHasRef(metrics.OriginPromAdapter, "container_cpu_usage_seconds_total", "", false, "container", "node", "pod")
}

func TestScanPromAdapterStandaloneAndFlaggerCanary(t *testing.T) {
	dir := t.TempDir()
	adapterFile := filepath.Join(dir, "adapter.yaml")
	if err := os.WriteFile(adapterFile, []byte(`
rules:
  - seriesQuery: 'custom_queue_depth{env="prod"}'
    metricsQuery: 'avg(<<.Series>>{<<.LabelMatchers>>}) by (<<.GroupBy>>)'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	canaryFile := filepath.Join(dir, "canary.yaml")
	if err := os.WriteFile(canaryFile, []byte(`
apiVersion: flagger.app/v1beta1
kind: Canary
metadata:
  name: podinfo
  namespace: test
spec:
  analysis:
    metrics:
      - name: inline-errors
        query: rate(canary_errors_total{namespace="{{ namespace }}"}[1m])
`), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(&config.K8sConsumersConfig{
		Globs: []string{dir},
	})
	if err != nil {
		t.Fatalf("Scan(dir) failed: %v", err)
	}
	if res.PromAdapterRuleCount != 1 || res.FlaggerTemplateCount != 1 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	foundDynAdapter := false
	foundCanary := false
	for _, ref := range res.Refs {
		if ref.Origin == metrics.OriginPromAdapter && ref.MetricName == "custom_queue_depth" && ref.PreservesAll {
			foundDynAdapter = true
		}
		if ref.Origin == metrics.OriginFlagger && ref.MetricName == "canary_errors_total" {
			foundCanary = true
		}
	}
	if !foundDynAdapter {
		t.Errorf("expected custom_queue_depth with PreservesAll=true when GroupBy has no explicit overrides")
	}
	if !foundCanary {
		t.Errorf("expected Canary inline metric canary_errors_total to be extracted")
	}

	// Missing glob fails closed
	if _, err := Scan(&config.K8sConsumersConfig{Globs: []string{filepath.Join(dir, "missing-*.yaml")}}); err == nil {
		t.Fatal("expected missing glob to fail closed")
	}

	// Feature toggle disables KEDA without touching Flagger
	falseVal := false
	toggled, err := Scan(&config.K8sConsumersConfig{
		Globs:              []string{canaryFile},
		EnableFlagger:      &falseVal,
		EnablePromAdapter:  &falseVal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if toggled.FlaggerTemplateCount != 0 || len(toggled.Refs) != 0 {
		t.Fatalf("expected EnableFlagger=false to suppress Flagger scanning, got %+v", toggled)
	}
}
