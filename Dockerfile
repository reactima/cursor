# Dockerfile
FROM golang:1.24-alpine3.20

RUN apk add --no-cache git ca-certificates && update-ca-certificates

WORKDIR /app

# go deps
COPY go.mod go.sum ./
RUN go mod download

# server source
COPY server.go ./
RUN go build -o ws-server server.go

# static assets built locally by `npm run build` → www/dist
COPY www/dist ./www/dist

EXPOSE 8989
ENTRYPOINT ["./ws-server"]
