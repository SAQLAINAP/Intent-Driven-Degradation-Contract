# All build output goes to bin/ (gitignored) so the repository root stays clean.
BIN_DIR   := bin
DG        := $(BIN_DIR)/dg
ENGINE    := $(BIN_DIR)/dg-engine
DEMO      := $(BIN_DIR)/demo-app
POLICY    := $(BIN_DIR)/policy.dg

EXAMPLE_POLICY := config/example-degradation.yaml
DEV_CONFIG     := config/dg-engine-dev.yaml
DEMO_PORT      ?= 8080
IMAGE          ?= iddc-demo

.DEFAULT_GOAL := help
.PHONY: help build build-engine build-demo build-all \
        test test-cover vet lint smoke \
        compile-example validate-example inspect-example graph-example simulate-critical version \
        run-dev run-demo demo docker-build docker-run observability-up observability-down clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ─── build ────────────────────────────────────────────────────────────────────

build: ## Build the dg CLI → bin/dg
	go build -o $(DG) ./cmd/dg

build-engine: ## Build the runtime engine → bin/dg-engine
	go build -o $(ENGINE) ./cmd/dg-engine

build-demo: ## Build the demo shop → bin/demo-app
	go build -o $(DEMO) ./cmd/demo-app

build-all: build build-engine build-demo ## Build all three binaries

# ─── quality ──────────────────────────────────────────────────────────────────

test: ## Unit tests with the race detector
	go test -race ./...

test-cover: ## Unit tests with a coverage summary
	go test -race -coverprofile=$(BIN_DIR)/coverage.out ./...
	go tool cover -func=$(BIN_DIR)/coverage.out | tail -1

vet: ## go vet all packages
	go vet ./...

lint: ## golangci-lint (must be installed)
	golangci-lint run ./...

smoke: ## End-to-end smoke test: CLI demo set + live engine and demo shop
	bash scripts/smoke.sh

# ─── policy tooling ───────────────────────────────────────────────────────────

compile-example: build ## Compile the example policy → bin/policy.dg
	$(DG) compile $(EXAMPLE_POLICY) -o $(POLICY)

validate-example: build ## Validate the example policy
	$(DG) validate $(EXAMPLE_POLICY)

inspect-example: compile-example ## Inspect the compiled example bundle
	$(DG) inspect $(POLICY)

graph-example: compile-example ## Print the example blast-radius graph
	$(DG) graph $(POLICY)

simulate-critical: build ## Simulate a critical-tier signal set
	$(DG) simulate $(EXAMPLE_POLICY) \
		--signal rps:2500 --signal pod_ceiling:0.95 --signal db_latency_p99:500

version: build ## Print the dg version
	$(DG) version

# ─── run ──────────────────────────────────────────────────────────────────────

demo: build-all compile-example ## Engine + demo shop (console :9191, shop :DEMO_PORT); Ctrl-C stops both
	PORT=$(DEMO_PORT) BIN=$(BIN_DIR) sh scripts/demo.sh

run-dev: build-engine compile-example ## Engine only, dev config (console http://localhost:9191/)
	$(ENGINE) --policy $(POLICY) --config $(DEV_CONFIG)

run-demo: build-demo ## Demo shop only (expects run-dev in another terminal)
	$(DEMO) --addr :$(DEMO_PORT) --sidecar http://localhost:8081 --engine http://localhost:9191

docker-build: ## Build the demo Docker image
	docker build -f deploy/docker/Dockerfile -t $(IMAGE) .

docker-run: docker-build ## Run the demo image (shop :8080, console :9191)
	docker run --rm -p 8080:8080 -p 9191:9191 $(IMAGE)

observability-up: ## Start Prometheus (:9091) + Grafana (:3000, admin/admin)
	docker compose -f deploy/observability/docker-compose.yaml up -d

observability-down: ## Stop the observability stack
	docker compose -f deploy/observability/docker-compose.yaml down

clean: ## Remove build output (including binaries left in the root by older layouts)
	rm -rf $(BIN_DIR)
	rm -f dg dg-engine demo-app policy.dg config/example-degradation.dg coverage.out
