BINARY := bin/trimtab
DEMO_NOW := 2026-11-21T12:00:00Z

.PHONY: build test lint verify helm-lint demo demo-otel demo-promrw demo-markdown clean kind-up kind-down kind-smoke tilt-up e2e-up e2e-build e2e-deploy e2e-seed e2e-test e2e-scenario e2e-artifacts e2e-down e2e-all

build:
	go build -o $(BINARY) ./cmd/trimtab

test:
	go test -race ./...

lint:
	golangci-lint run ./...

verify:
	go test -race ./... && go build ./cmd/trimtab

helm-lint:
	helm lint charts/trimtab
	helm template trimtab charts/trimtab >/dev/null
	helm lint charts/trimtab -f charts/trimtab/examples/values-nonprod.yaml
	helm lint charts/trimtab -f charts/trimtab/examples/values-prod.yaml
	helm lint charts/trimtab -f charts/trimtab/examples/values-otel.yaml
	helm lint charts/trimtab -f charts/trimtab/examples/values-promrw.yaml
	helm template trimtab charts/trimtab -f charts/trimtab/examples/values-nonprod.yaml >/dev/null
	helm template trimtab charts/trimtab -f charts/trimtab/examples/values-prod.yaml >/dev/null
	helm template trimtab charts/trimtab -f charts/trimtab/examples/values-otel.yaml >/dev/null
	helm template trimtab charts/trimtab -f charts/trimtab/examples/values-promrw.yaml >/dev/null

demo: build
	./$(BINARY) analyze --config examples/demo/trimtab.yaml --now $(DEMO_NOW) --no-state-write
	./$(BINARY) synth --config examples/demo/trimtab.yaml --preview-next --now $(DEMO_NOW) --no-state-write

demo-otel: build
	./$(BINARY) analyze --config examples/demo/trimtab-otel.yaml --now $(DEMO_NOW) --no-state-write
	./$(BINARY) synth --config examples/demo/trimtab-otel.yaml --preview-next --now $(DEMO_NOW) --no-state-write

demo-promrw: build
	./$(BINARY) analyze --config examples/demo/trimtab-promrw.yaml --now $(DEMO_NOW) --no-state-write
	./$(BINARY) synth --config examples/demo/trimtab-promrw.yaml --preview-next --now $(DEMO_NOW) --no-state-write

demo-markdown: build
	./$(BINARY) analyze --config examples/demo/trimtab.yaml --format markdown --now $(DEMO_NOW) --no-state-write

kind-up:
	./scripts/local-k8s.sh up

kind-down:
	./scripts/local-k8s.sh down

kind-smoke:
	./scripts/local-k8s.sh smoke

tilt-up:
	tilt up

e2e-up:
	./test/e2e/e2e.sh up

e2e-build:
	./test/e2e/e2e.sh build

e2e-deploy:
	./test/e2e/e2e.sh deploy

e2e-seed:
	./test/e2e/e2e.sh seed

e2e-test:
	./test/e2e/e2e.sh test

e2e-scenario:
	@test -n "$(S)" || (echo "Usage: make e2e-scenario S=S0" >&2; exit 2)
	./test/e2e/e2e.sh scenario "$(S)"

e2e-artifacts:
	./test/e2e/e2e.sh artifacts

e2e-down:
	./test/e2e/e2e.sh down

e2e-all:
	./test/e2e/e2e.sh all

clean:
	rm -rf bin/ trimtab out/ dist/ examples/demo/out/ examples/demo/.trimtab-state*.json
