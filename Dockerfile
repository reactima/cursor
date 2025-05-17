# Dockerfile
FROM golang:1.24-alpine3.20

# Install git (for modules) and ca-certificates (for HTTPS)
RUN apk add --no-cache git ca-certificates && update-ca-certificates

WORKDIR /app

# Copy go.mod/ go.sum first to cache modules download
COPY go.mod go.sum ./
RUN go mod download

# Copy the server source
COPY server.go ./

# Build the binary
RUN go build -o ws-server server.go

# Expose port and run
EXPOSE 8989
ENTRYPOINT ["./ws-server"]
