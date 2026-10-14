.PHONY: run-api run-local import fmt tidy test vet build

GO_TAGS ?= sqlite_fts5
CSV ?= ./datasets/ecdict.csv
BIN ?= ./bin/api

run-api:
	go run -tags "$(GO_TAGS)" ./cmd/api

# Same as run-api, but loads secrets (LLM / Feishu / Qiniu keys) from ./.env
run-local:
	set -a; [ -f .env ] && . ./.env; set +a; go run -tags "$(GO_TAGS)" ./cmd/api

import:
	@if [ ! -f "$(CSV)" ]; then echo "CSV not found: $(CSV)"; echo "Put ECDICT at ./datasets/ecdict.csv or pass CSV=/path/to/ecdict.csv"; exit 1; fi
	go run -tags "$(GO_TAGS)" ./cmd/importer -csv "$(CSV)"

test:
	go test -tags "$(GO_TAGS)" -race ./...

vet:
	go vet -tags "$(GO_TAGS)" ./...

build:
	go build -trimpath -tags "$(GO_TAGS)" -o "$(BIN)" ./cmd/api

fmt:
	gofmt -w ./cmd ./internal ./migrations

tidy:
	go mod tidy
