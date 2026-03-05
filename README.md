# gogoga_dictionary

English | [简体中文](./README.zh-CN.md)

A Go-based dictionary web service powered by ECDICT, designed for iOS/APP word lookup scenarios.

## Features

- `GET /v1/word/{word}`: exact word lookup
- `GET /v1/search?q=...&mode=prefix|fuzzy&page=1&page_size=20`: search (prefix/fuzzy)
- `GET /v1/suggest?q=...&limit=10`: autocomplete suggestions
- `GET /v1/health`: health check

## Project Structure

- `cmd/api`: API server entrypoint
- `cmd/importer`: ECDICT CSV importer entrypoint
- `internal/db`: SQLite bootstrap and schema setup
- `internal/repo`: dictionary data access layer
- `internal/http`: HTTP handlers
- `migrations/schema.sql`: DB schema and indexes
- `datasets/`: dictionary dataset directory (recommended: `ecdict.csv`)

## Dataset Location

Recommended path:

```bash
./datasets/ecdict.csv
```

This repository is configured to allow committing this file for easier onboarding.

## Local Development

1. Install dependencies

```bash
go mod tidy
```

2. Import ECDICT CSV (default path: `./datasets/ecdict.csv`)

```bash
make import
```

Or provide a custom path:

```bash
make import CSV=/absolute/path/to/ecdict.csv
```

3. Start API service

```bash
make run-api
```

Defaults:

- HTTP listen: `:8080`
- DB file: `./data/dict.db`
- Build tag in Makefile: `sqlite_fts5` (required for FTS-based fuzzy search)

## Docker Deployment

1. Prepare local directories and dataset

```bash
mkdir -p datasets data
# put ecdict.csv into ./datasets/ecdict.csv
```

2. Build image

```bash
docker compose build
```

3. Import dictionary data (one-time or when dataset updates)

```bash
docker compose --profile tools run --rm importer
```

4. Start API service

```bash
docker compose up -d api
```

5. Verify

```bash
curl 'http://127.0.0.1:8080/v1/health'
```

## Environment Variables

- `HTTP_ADDR` (default: `:8080`)
- `DB_PATH` (default: `./data/dict.db`, container default: `/app/data/dict.db`)
- `SCHEMA_PATH` (default: `./migrations/schema.sql`, container default: `/app/migrations/schema.sql`)

## API Examples

```bash
curl 'http://127.0.0.1:8080/v1/word/apple'
curl 'http://127.0.0.1:8080/v1/suggest?q=app&limit=5'
curl 'http://127.0.0.1:8080/v1/search?q=apple&mode=prefix&page=1&page_size=20'
curl 'http://127.0.0.1:8080/v1/search?q=network security&mode=fuzzy&page=1&page_size=20'
```

## Expected ECDICT CSV Columns

The importer reads columns by header name (case-insensitive). Recommended columns:

- `word`
- `phonetic`
- `definition`
- `translation`
- `pos`
- `collins`
- `oxford`
- `tag`
- `bnc`
- `frq`
- `exchange`
- `detail`
- `audio`

Rows with empty `word` are skipped. Missing columns are treated as empty values.
