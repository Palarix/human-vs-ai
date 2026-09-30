APP := human-vs-ai
GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build run install test test-race vet fmt fmt-check check clean help

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .

run:
	$(GO) run . $(ARGS)

install:
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags "$(LDFLAGS)" .

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w *.go

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { gofmt -l *.go; exit 1; }

check: fmt-check vet test-race

clean:
	rm -f $(APP) $(APP).exe
	rm -rf dist stage

help:
	@printf '%s\n' \
		'build       Build a static local binary (default)' \
		'run         Run the CLI; append arguments with ARGS=...' \
		'install     Install the binary with go install' \
		'test        Run unit and integration tests' \
		'test-race   Run tests with the race detector' \
		'vet         Run go vet' \
		'fmt         Format Go source files' \
		'fmt-check   Check Go source formatting' \
		'check       Run formatting, vet, and race tests' \
		'clean       Remove local build and release output'
