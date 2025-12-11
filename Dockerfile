FROM golang:1.24-bullseye AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    pkg-config \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=1
RUN go build -o maxapi

FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    netcat-openbsd \
    postgresql-client \
    openssl \
    curl \
    ffmpeg \
    tzdata \
    && rm -rf /var/lib/apt/lists/*

ENV TZ="America/Sao_Paulo"
WORKDIR /app

COPY --from=builder /app/maxapi         /app/
COPY --from=builder /app/static         /app/static/
COPY --from=builder /app/maxapi.service /app/maxapi.service

RUN chmod +x /app/maxapi && \
    chmod -R 755 /app && \
    chown -R root:root /app

ENTRYPOINT ["/app/maxapi", "--logtype=console", "--color=true"]
