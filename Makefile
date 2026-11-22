BINARY := bin/trimtab
DEMO_NOW := 2026-11-21T12:00:00Z

.PHONY: build test verify demo demo-otel demo-markdown clean

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

demo-markdown: build
	./$(BINARY) analyze --config examples/demo/trimtab.yaml --format markdown --now $(DEMO_NOW) --no-state-write

clean:
	rm -rf bin/ trimtab out/ examples/demo/out/ examples/demo/.trimtab-state.json
