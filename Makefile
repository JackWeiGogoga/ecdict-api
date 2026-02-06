.PHONY: run-api import fmt tidy

GO_TAGS ?= sqlite_fts5
CSV ?= ./datasets/ecdict.csv

run-api:
	go run -tags "$(GO_TAGS)" ./cmd/api

import:
	@if [ ! -f "$(CSV)" ]; then echo "CSV not found: $(CSV)"; echo "Put ECDICT at ./datasets/ecdict.csv or pass CSV=/path/to/ecdict.csv"; exit 1; fi
	go run -tags "$(GO_TAGS)" ./cmd/importer -csv "$(CSV)"

fmt:
	gofmt -w ./cmd ./internal

tidy:
	go mod tidy
