FROM golang:1.25-alpine AS builder

# VERSION is stamped into main.version (reported by /api/version and the
# startup log). CI passes the short commit SHA; local builds report "dev".
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /meshsat-hub ./cmd/meshsat-hub/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /meshsat-hub /usr/local/bin/meshsat-hub
COPY assets/msvqsc/ /data/msvqsc/

EXPOSE 6070
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:6070/healthz || exit 1

ENTRYPOINT ["meshsat-hub"]
