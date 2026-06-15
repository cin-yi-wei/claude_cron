BINARY := claude-cron
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build install dist clean

test:
	go test ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/claude-cron

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/claude-cron

dist:
	./scripts/build-release.sh

clean:
	rm -rf bin dist
