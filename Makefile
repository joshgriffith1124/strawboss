GO ?= go

.PHONY: build build-all test race fmt vet check

# CGO_ENABLED=0 is pinned, not inherited. strawboss needs no cgo, and the
# default flips with whether a C compiler happens to be installed — which
# silently changed the build the day gcc appeared. A pinned value also
# rules cgo out of "incorrect use of unsafe or cgo?" heap-corruption
# reports (docs/NOTES.md) and yields a static binary.
build:
	CGO_ENABLED=0 $(GO) build -o bin/strawboss ./cmd/strawboss

# Both binaries for the hosts actually in use: this box and the Mac.
build-all: build
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o bin/strawboss-darwin-arm64 ./cmd/strawboss

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
