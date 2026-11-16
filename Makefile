GO ?= go

export GOTOOLCHAIN := local
export GOWORK := off
export GOMAXPROCS := 2

.PHONY: test test-race build release

test:
	$(GO) test -mod=readonly -p 2 ./...

test-race:
	CGO_ENABLED=1 BOOKWAREHOUSE_TEST_RACE=1 $(GO) test -mod=readonly -race -p 2 ./...

build:
	GO="$(GO)" ./scripts/build.sh

release:
	GO="$(GO)" ./scripts/build.sh --all
