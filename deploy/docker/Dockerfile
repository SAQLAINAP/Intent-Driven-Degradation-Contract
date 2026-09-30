# IDDC image: dg, dg-engine and the demo shop, plus the compiled example policy.
# The default command runs the demo (engine + shop). To run only the engine:
#   docker run ghcr.io/saqlainap/iddc bin/dg-engine --policy /etc/dg/policy.dg --config /etc/dg/dg-engine.yaml
# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile -t iddc-demo .
#   docker run -p 8080:8080 -p 9191:9191 iddc-demo
FROM golang:1.27 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=""
RUN X=github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/internal/version && \
    LDFLAGS="-s -w -X $X.Version=$VERSION -X $X.Commit=$COMMIT -X $X.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/dg        ./cmd/dg        && \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/dg-engine ./cmd/dg-engine && \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/demo-app  ./cmd/demo-app  && \
    /out/dg compile config/example-degradation.yaml -o /out/policy.dg

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract" \
      org.opencontainers.image.description="IDDC — Intent-Driven Degradation Contracts (engine, CLI and demo shop)" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/          ./bin/
COPY config/                       ./config/
COPY scripts/demo.sh               ./demo.sh
RUN chmod +x demo.sh && useradd --system --uid 10001 iddc
USER iddc

# 8080 = demo shop, 9191 = IDDC web console / control API
EXPOSE 8080 9191
CMD ["./demo.sh"]
