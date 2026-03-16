# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy mod and sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o roomws main.go

# Final stage
FROM scratch

WORKDIR /

# Copy binary from builder
COPY --from=builder /app/roomws /roomws

# Expose default port
EXPOSE 8080

ENTRYPOINT ["/roomws"]
