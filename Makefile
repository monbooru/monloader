.PHONY: build build-tray lint test test-live coverage

LDFLAGS := -ldflags="$(shell . ./packaging/release-env.sh && echo "$$LDFLAGS")"

build:
	go build $(LDFLAGS) ./cmd/monloader

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monloader

lint:
	golangci-lint run

test:
	go test -race ./...

# The live cases reach the real sites.
test-live:
	go test -tags live -race -run Live ./...

coverage:
	go test -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html
