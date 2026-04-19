.PHONY: build build-engine build-demo build-all test lint clean compile-example inspect-example run-dev run-demo observability-up observability-down

BINARY        := dg
ENGINE_BINARY := dg-engine
DEMO_BINARY   := demo-app
CMD           := ./cmd/dg
ENGINE_CMD    := ./cmd/dg-engine
DEMO_CMD      := ./cmd/demo-app

# Build the dg CLI binary
build:
	go build -o $(BINARY) $(CMD)

# Build the dg-engine runtime binary
build-engine:
	go build -o $(ENGINE_BINARY) $(ENGINE_CMD)

# Build the demo-app binary
build-demo:
	go build -o $(DEMO_BINARY) $(DEMO_CMD)

# Build all binaries
build-all: build build-engine build-demo

# Run all tests with race detector
test:
	go test -race ./...

# Run linter (requires golangci-lint)
lint:
	golangci-lint run ./...

# Compile the example policy
compile-example: build
	./$(BINARY) compile config/example-degradation.yaml -o policy.dg

# Inspect the compiled example bundle
inspect-example: compile-example
	./$(BINARY) inspect policy.dg

# Validate only (no binary emitted)
validate-example: build
	./$(BINARY) validate config/example-degradation.yaml

# Simulate a critical tier scenario
simulate-critical: build
	./$(BINARY) simulate config/example-degradation.yaml \
		--signal rps:2500 --signal pod_ceiling:0.95 --signal db_latency_p99:500

# Print blast radius graph
graph-example: compile-example
	./$(BINARY) graph policy.dg

# Show version
version: build
	./$(BINARY) version

# Start the engine in dev mode (fast hysteresis, signal injection enabled)
# Requires a compiled policy: make compile-example first
run-dev: build-engine compile-example
	./$(ENGINE_BINARY) \
		--policy config/example-degradation.dg \
		--config config/dg-engine-dev.yaml

# Run the demo app (engine must already be running via make run-dev)
run-demo: build-demo
	./$(DEMO_BINARY) --addr :8080 --sidecar http://localhost:8081

# Start Grafana + Prometheus observability stack (requires Docker Desktop)
# Grafana: http://localhost:3000 (admin/admin)
# Prometheus: http://localhost:9091
observability-up:
	docker compose up -d

# Stop observability stack
observability-down:
	docker compose down

clean:
	rm -f $(BINARY) $(ENGINE_BINARY) $(DEMO_BINARY) policy.dg config/example-degradation.dg coverage.out
