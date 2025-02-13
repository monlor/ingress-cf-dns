# Build stage
FROM --platform=$BUILDPLATFORM golang:1.21-alpine AS builder

ARG TARGETARCH

WORKDIR /app

# Copy go mod files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with optimizations
RUN GOARCH=${TARGETARCH} CGO_ENABLED=0 GOOS=linux go build \
        -ldflags="-s -w" \
        -o ingress-cf-dns

# Final stage
FROM scratch

# Copy SSL certificates for HTTPS support
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/ingress-cf-dns /

ENTRYPOINT ["/ingress-cf-dns"] 