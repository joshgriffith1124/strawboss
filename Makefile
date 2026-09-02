GO ?= go

.PHONY: build test race fmt vet check

build:
	$(GO) build -o bin/strawboss ./cmd/strawboss

test:
	$(GO) test ./...

# The race detector needs cgo, so it is a separate target rather than a
# flag on `test` — a box without a C compiler must still be able to run
# the suite. Concurrency bugs here are crashes, not wrong output, so run
# this before shipping anything that touches the feeds.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

fmt:
	gofmt -l -w .
	$(GO) vet ./...

check: fmt test build
