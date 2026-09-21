.PHONY: build build-arm64 build-x86_64 build-sim test test-integration lint fmt clean docker run dev security gosec govulncheck owasp owasp-full swagger

BINARY := meshsat-hub
PKG := github.com/meshsat/meshsat-hub
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/meshsat-hub/

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-arm64 ./cmd/meshsat-hub/

build-x86_64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-amd64 ./cmd/meshsat-hub/

test:
	CGO_ENABLED=0 go test -v -count=1 ./...

test-integration:
	CGO_ENABLED=0 go test -v -count=1 -tags=integration ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .
	@if [ -n "$$(gofmt -l .)" ]; then echo "gofmt found unformatted files"; exit 1; fi

security: gosec govulncheck

gosec:
	gosec ./...

govulncheck:
	govulncheck ./...

owasp:
	@echo "Running OWASP baseline scan (set HUB_TARGET_URL and HUB_AUTH_TOKEN)..."
	bash test/owasp/owasp-scan.sh

owasp-full:
	@echo "Running OWASP full active scan (set HUB_TARGET_URL and HUB_AUTH_TOKEN)..."
	bash test/owasp/owasp-scan.sh --full

# The generator is PINNED, and run through `go run` so whatever swag happens to be
# on PATH is never used: the CI `swagger` job fails on any difference from the
# committed spec, and a different swag version emits a different spec (v1.16.6
# adds x-enum-descriptions and int64 formats that v1.16.4 does not). Keep this
# version equal to the one in .gitlab-ci.yml.
SWAG_VERSION := v1.16.4
swagger:
	go run github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION) init -g cmd/meshsat-hub/main.go -o docs/swagger --parseDependency --parseInternal
	rm -f docs/swagger/docs.go

build-sim:
	CGO_ENABLED=0 go build -o bin/meshsat-sim ./cmd/meshsat-sim/

clean:
	rm -rf bin/

docker:
	docker build --build-arg VERSION=$(VERSION) -t meshsat-hub:latest .

run:
	HUB_LOG_FORMAT=text HUB_LOG_LEVEL=debug go run ./cmd/meshsat-hub/

dev:
	@echo "Starting MeshSat Hub dev environment (Hub + MQTT + Simulator)..."
	docker compose up -d mqtt
	@sleep 2
	@echo "Starting Hub in background..."
	HUB_LOG_FORMAT=text HUB_LOG_LEVEL=debug HUB_AUTH_MODE=none go run ./cmd/meshsat-hub/ &
	@sleep 3
	@echo "Starting Simulator (3 devices, 30s interval)..."
	go run ./cmd/meshsat-sim/ --hub-url http://localhost:6070 --devices 3 --interval 30s
