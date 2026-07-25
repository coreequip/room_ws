# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy mod and sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-w -s" -o roomws main.go

# Final stage
FROM scratch

WORKDIR /

# Copy binary from builder
COPY --from=builder /app/roomws /roomws

# Run as a non-root, unprivileged UID. FROM scratch has no /etc/passwd, but a
# numeric UID needs no NSS lookup and the kernel enforces it regardless.
USER 65532:65532

# Expose default port
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/roomws", "-health"]

ENTRYPOINT ["/roomws"]
