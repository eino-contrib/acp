.PHONY: gen gen-refresh test test-race vet ci build run-agent run-client run-stdio run-http run-ws run-proxy

GO ?= go
AGENT_ADDR ?= :18080
PROXY_LISTEN ?= :8080
PROXY_AGENT_LISTEN ?= :9090
HTTP_FRAMEWORK ?= hertz

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
	./bin/agent -transport=http -http-framework=$(HTTP_FRAMEWORK) -listen=$(AGENT_ADDR)

run-client: build
	./bin/client -transport=ws ws://localhost$(AGENT_ADDR)

run-stdio: build
	@echo "Starting client (stdio/spawn) ..."
	@./bin/client -transport=spawn ./bin/agent

run-http: build
	@set -e; \
		agent_pid=; \
		cleanup() { status=$$?; trap - EXIT INT TERM; if [ -n "$$agent_pid" ]; then kill "$$agent_pid" 2>/dev/null || true; wait "$$agent_pid" 2>/dev/null || true; fi; exit $$status; }; \
		trap cleanup EXIT INT TERM; \
		echo "Starting agent on $(AGENT_ADDR) ..."; \
		./bin/agent -transport=http -http-framework=$(HTTP_FRAMEWORK) -listen=$(AGENT_ADDR) & \
		agent_pid=$$!; \
		sleep 1; \
		echo "Starting client ..."; \
		./bin/client -transport=http http://localhost$(AGENT_ADDR)

run-ws: build
	@set -e; \
		agent_pid=; \
		cleanup() { status=$$?; trap - EXIT INT TERM; if [ -n "$$agent_pid" ]; then kill "$$agent_pid" 2>/dev/null || true; wait "$$agent_pid" 2>/dev/null || true; fi; exit $$status; }; \
		trap cleanup EXIT INT TERM; \
		echo "Starting agent on $(AGENT_ADDR) ..."; \
		./bin/agent -transport=http -http-framework=$(HTTP_FRAMEWORK) -listen=$(AGENT_ADDR) & \
		agent_pid=$$!; \
		sleep 1; \
		echo "Starting client ..."; \
		./bin/client -transport=ws ws://localhost$(AGENT_ADDR)

# run-proxy brings up the full Client → Proxy → AgentServer → Agent chain in
# one shot. The proxy binary runs with -role=all so both the proxy (on
# PROXY_LISTEN) and the example agent-server (on PROXY_AGENT_LISTEN) live in
# the same process. The example client then connects to the proxy at /acp,
# completely unaware of the agent-server's existence.
run-proxy: build
	@set -e; \
		proxy_pid=; \
		cleanup() { status=$$?; trap - EXIT INT TERM; if [ -n "$$proxy_pid" ]; then kill "$$proxy_pid" 2>/dev/null || true; wait "$$proxy_pid" 2>/dev/null || true; fi; exit $$status; }; \
		trap cleanup EXIT INT TERM; \
		echo "Starting proxy (role=all) on $(PROXY_LISTEN); upstream agent-server on $(PROXY_AGENT_LISTEN) ..."; \
		./bin/proxy -role=all -http-framework=$(HTTP_FRAMEWORK) -proxy-listen=$(PROXY_LISTEN) -agent-listen=$(PROXY_AGENT_LISTEN) & \
		proxy_pid=$$!; \
		sleep 1; \
		echo "Starting client ..."; \
		./bin/client -transport=ws ws://localhost$(PROXY_LISTEN)
