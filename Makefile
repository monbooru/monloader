.PHONY: build build-tray lint

VERSION  := $(shell cat VERSION.md 2>/dev/null | tr -d '[:space:]')
REPO_URL := $(shell cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]')
DOC_URL  := $(shell cat DOC.md 2>/dev/null | tr -d '[:space:]')
LDFLAGS  := -ldflags="-X 'github.com/monbooru/monloader/internal/web.Version=$(VERSION)' -X 'github.com/monbooru/monloader/internal/web.RepoURL=$(REPO_URL)' -X 'github.com/monbooru/monloader/internal/web.DocURL=$(DOC_URL)'"

build:
	go build $(LDFLAGS) ./cmd/monloader

build-tray:
	go build -tags tray $(LDFLAGS) ./cmd/monloader

lint:
	golangci-lint run
