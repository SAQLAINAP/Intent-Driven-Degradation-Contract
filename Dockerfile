FROM golang:1.26 AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o dg       ./cmd/dg       && \
    go build -o dg-engine ./cmd/dg-engine && \
    go build -o demo-app  ./cmd/demo-app  && \
    ./dg compile config/example-degradation.yaml -o policy.dg

FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/dg-engine  .
COPY --from=builder /app/demo-app   .
COPY --from=builder /app/policy.dg  .
COPY --from=builder /app/config/    ./config/
COPY start.sh .
RUN chmod +x start.sh

EXPOSE 8080
CMD ["./start.sh"]
