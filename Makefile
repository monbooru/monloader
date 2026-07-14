.PHONY: build test lint coverage ptr-smoke

VERSION  := $(shell cat VERSION.md 2>/dev/null | tr -d '[:space:]')
REPO_URL := $(shell cat REPOSITORY.md 2>/dev/null | tr -d '[:space:]')
DOC_URL  := $(shell cat DOC.md 2>/dev/null | tr -d '[:space:]')
LDFLAGS  := -ldflags="-X 'github.com/leqwin/monloader/internal/web.Version=$(VERSION)' -X 'github.com/leqwin/monloader/internal/web.RepoURL=$(REPO_URL)' -X 'github.com/leqwin/monloader/internal/web.DocURL=$(DOC_URL)'"

build:
	go build $(LDFLAGS) ./cmd/monloader

test:
	go test -race ./...

lint:
	golangci-lint run

coverage:
	go test -coverprofile=coverage.out $(shell go list ./... | grep -v '/cmd/')
	go tool cover -html=coverage.out -o coverage.html

# Live smoke test against the real Hydrus PTR 
ptr-smoke:
	go test -tags ptrsmoke -run TestPTRSmoke -v ./internal/ptr
