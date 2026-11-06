BINARY := lgllm
CMD     := ./cmd/lgllm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X lgllm/internal/version.Version=$(VERSION) \
	-X lgllm/internal/version.Commit=$(COMMIT) \
	-X lgllm/internal/version.Date=$(DATE)

.PHONY: all build run test race vet fmt check clean install tidy snapshot bench image-size

all: check build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(CMD)

install:
	CGO_ENABLED=0 go install -trimpath -ldflags '$(LDFLAGS)' $(CMD)

run:
	go run $(CMD) -config config.yaml

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# check is the gate used before a commit and in CI.
check: fmt vet test

clean:
	rm -rf bin

tidy:
	go mod tidy

# Size and startup baseline for the lightweight goal (phase 11 tracks trends).
size: build
	@ls -lh bin/$(BINARY) | awk '{print "binary:", $$5}'

# Release snapshot: cross-compiles every target without publishing anything.
snapshot:
	goreleaser release --snapshot --clean

# Local numbers: binary, cold start, RSS, latency, throughput.
bench:
	python3 scripts/bench.py

# Scratch image size, the "image < 30 MB" target.
image-size:
	@docker build -q -t lgllm:dev . >/dev/null
	@docker image inspect lgllm:dev --format '{{.Size}}' | awk '{printf "image: %.1f MB\n", $$1/1024/1024}'
