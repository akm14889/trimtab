BINARY := bin/trimtab
DEMO_NOW := 2026-11-21T12:00:00Z

.PHONY: build test verify demo demo-otel demo-promrw demo-markdown clean kind-up kind-down kind-smoke tilt-up

build:
	go build -o $(BINARY) ./cmd/trimtab

test:
	go test -race ./...

verify:
	go test -race ./... && go build ./cmd/trimtab

demo: build
	./$(BINARY) analyze --config examples/demo/trimtab.yaml --now $(DEMO_NOW) --no-state-write

demo-otel: build
	./$(BINARY) analyze --config examples/demo/trimtab-otel.yaml --now $(DEMO_NOW) --no-state-write

demo-promrw: build
	./$(BINARY) analyze --config examples/demo/trimtab-promrw.yaml --now $(DEMO_NOW) --no-state-write

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

clean:
	rm -rf bin/ trimtab out/ examples/demo/out/ examples/demo/.trimtab-state*.json
