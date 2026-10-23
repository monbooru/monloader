.PHONY: build build-tray lint

LDFLAGS := -ldflags="$(shell . ./packaging/release-env.sh && echo "$$LDFLAGS")"

build:
	go build $(LDFLAGS) ./cmd/monloader

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monloader

lint:
	golangci-lint run
