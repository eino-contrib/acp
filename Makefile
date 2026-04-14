.PHONY: gen test test-race vet ci build run-agent run-client run-ws

GO ?= go
AGENT_ADDR ?= :18080

gen:
	$(GO) run ./cmd/generate \
		-schema ./cmd/generate/schema/schema.json \
		-meta ./cmd/generate/schema/meta.json \
		-output ./types_gen.go \
		-package acp \
		-download=false

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

ci: vet test test-race

build:
	@mkdir -p bin
	$(GO) build -o bin/agent ./examples/agent
	$(GO) build -o bin/client ./examples/client

run-agent: build
	./bin/agent -transport=http -listen=$(AGENT_ADDR)

run-client: build
	./bin/client -transport=ws ws://localhost$(AGENT_ADDR)/acp

run-ws: build
	@-lsof -t -i $(AGENT_ADDR) | xargs -r kill -9 2>/dev/null
	@echo "Starting agent on $(AGENT_ADDR) ..."
	@./bin/agent -transport=http -listen=$(AGENT_ADDR) &
	@sleep 1
	@echo "Starting client ..."
	@./bin/client -transport=ws ws://localhost$(AGENT_ADDR)/acp; \
	EXIT_CODE=$$?; \
	kill %1 2>/dev/null; \
	exit $$EXIT_CODE
