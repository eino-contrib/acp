.PHONY: gen gen-refresh test test-race vet ci build run-agent run-client run-stdio run-http run-ws run-proxy

GO ?= go
AGENT_ADDR ?= :18080
PROXY_LISTEN ?= :8080
PROXY_AGENT_LISTEN ?= :9090

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
	$(GO) build -o bin/proxy ./examples/proxy

run-agent: build
	./bin/agent -transport=http -listen=$(AGENT_ADDR)

run-client: build
	./bin/client -transport=ws ws://localhost$(AGENT_ADDR)

run-stdio: build
	@echo "Starting client (stdio/spawn) ..."
	@./bin/client -transport=spawn ./bin/agent

run-http: build
	@-lsof -t -i $(AGENT_ADDR) | xargs -r kill -9 2>/dev/null
	@echo "Starting agent on $(AGENT_ADDR) ..."
	@./bin/agent -transport=http -listen=$(AGENT_ADDR) &
	@sleep 1
	@echo "Starting client ..."
	@./bin/client -transport=http http://localhost$(AGENT_ADDR); \
	EXIT_CODE=$$?; \
	kill %1 2>/dev/null; \
	exit $$EXIT_CODE

run-ws: build
	@-lsof -t -i $(AGENT_ADDR) | xargs -r kill -9 2>/dev/null
	@echo "Starting agent on $(AGENT_ADDR) ..."
	@./bin/agent -transport=http -listen=$(AGENT_ADDR) &
	@sleep 1
	@echo "Starting client ..."
	@./bin/client -transport=ws ws://localhost$(AGENT_ADDR); \
	EXIT_CODE=$$?; \
	kill %1 2>/dev/null; \
	exit $$EXIT_CODE

# run-proxy brings up the full Client → Proxy → AgentServer → Agent chain in
# one shot. The proxy binary runs with -role=all so both the proxy (on
# PROXY_LISTEN) and the example agent-server (on PROXY_AGENT_LISTEN) live in
# the same process. The example client then connects to the proxy at /acp,
# completely unaware of the agent-server's existence.
run-proxy: build
	@-lsof -t -i $(PROXY_LISTEN) | xargs -r kill -9 2>/dev/null
	@-lsof -t -i $(PROXY_AGENT_LISTEN) | xargs -r kill -9 2>/dev/null
	@echo "Starting proxy (role=all) on $(PROXY_LISTEN); upstream agent-server on $(PROXY_AGENT_LISTEN) ..."
	@./bin/proxy -role=all -proxy-listen=$(PROXY_LISTEN) -agent-listen=$(PROXY_AGENT_LISTEN) &
	@sleep 1
	@echo "Starting client ..."
	@./bin/client -transport=ws ws://localhost$(PROXY_LISTEN); \
	EXIT_CODE=$$?; \
	kill %1 2>/dev/null; \
	exit $$EXIT_CODE
