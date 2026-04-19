.PHONY: gen gen-refresh test test-race vet ci build run-agent run-client run-ws

GO ?= go
AGENT_ADDR ?= :18080

# gen regenerates the SDK from the checked-in schema snapshots under
# cmd/generate/schema/. This is deterministic and offline — CI and local
# development share the exact same inputs. Use gen-refresh to pull the
# latest schema from upstream before generating.
gen:
	$(GO) run ./cmd/generate \
		-output ./types_gen.go \
		-package acp \
		-download=false

# gen-refresh downloads the latest upstream schema/meta files into
# cmd/generate/schema/ and then regenerates. Commit the refreshed schema
# files alongside the generated Go code so subsequent `make gen` runs
# reproduce the same output.
gen-refresh:
	$(GO) run ./cmd/generate \
		-output ./types_gen.go \
		-package acp \
		-download=true

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

ci: vet test test-race

build:
	@mkdir -p bin
	$(GO) generate ./...
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
