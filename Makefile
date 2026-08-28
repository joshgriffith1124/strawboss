GO ?= go

.PHONY: build test fmt vet check

build:
	$(GO) build -o bin/strawboss ./cmd/strawboss

test:
	$(GO) test ./...

fmt:
	gofmt -l -w .
	$(GO) vet ./...

check: fmt test build
