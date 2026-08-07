BINARY := claude-cron
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build install dist clean a2a-image

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

A2A_IMAGE ?= cc-a2a-sandbox:1

a2a-image:
	docker build --platform linux/arm64 -t $(A2A_IMAGE) docker/a2a-sandbox
