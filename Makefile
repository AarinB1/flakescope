# `make check` is the gate every commit must pass (see CLAUDE.md).
#
# It deliberately does not run anything under testdata/: the go tool ignores
# directories named testdata when matching ./..., and testdata/flakypkg
# contains tests that are supposed to fail.

GO ?= go

.PHONY: check fmt vet test build clean

check: fmt vet test

fmt:
	@out="$$(gofmt -l . 2>&1)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needs to be run on:"; echo "$$out"; exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -race -count=1

build:
	$(GO) build ./...

clean:
	$(GO) clean
	rm -f flakescope
