FROM golang:1.25-alpine AS builder

# VERSION is stamped into main.version (reported by /api/version and the
# startup log). CI passes the short commit SHA; local builds report "dev".
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /meshsat-hub ./cmd/meshsat-hub/
# The tak-operator ships in the same image and is selected by the Deployment's
# command, the way the OpenTAKServer image runs three programs. One image means
# one build, one digest, one CVE scan — and the operator and the Hub are always
# the same commit, so the custom resources they share cannot drift apart.
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /tak-operator ./cmd/tak-operator/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /meshsat-hub /usr/local/bin/meshsat-hub
COPY --from=builder /tak-operator /usr/local/bin/tak-operator
COPY assets/msvqsc/ /data/msvqsc/

# The same uid the k8s manifest already runs the container as (runAsUser
# 65532). Declared here too so the image is non-root wherever it runs, and so
# the IaC gate (trivy DS002) can see it.
USER 65532:65532

EXPOSE 6070
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:6070/healthz || exit 1

ENTRYPOINT ["meshsat-hub"]
