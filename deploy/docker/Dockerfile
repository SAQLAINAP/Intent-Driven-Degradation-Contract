# IDDC demo image: dg-engine + demo shop + compiled example policy.
# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile -t iddc-demo .
#   docker run -p 8080:8080 -p 9191:9191 iddc-demo
FROM golang:1.26 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/dg        ./cmd/dg        && \
    go build -o /out/dg-engine ./cmd/dg-engine && \
    go build -o /out/demo-app  ./cmd/demo-app  && \
    /out/dg compile config/example-degradation.yaml -o /out/policy.dg

FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/          ./bin/
COPY config/                       ./config/
COPY scripts/demo.sh               ./demo.sh
RUN chmod +x demo.sh

# 8080 = demo shop, 9191 = IDDC web console / control API
EXPOSE 8080 9191
CMD ["./demo.sh"]
