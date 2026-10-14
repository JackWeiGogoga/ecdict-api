# gogoga_dictionary

English | [简体中文](./README.zh-CN.md)

A Go-based dictionary web service powered by ECDICT, designed for iOS/APP word lookup scenarios.

## Features

- `GET /v1/word/{word}`: exact word lookup
- `GET /v1/search?q=...&mode=prefix|fuzzy&page=1&page_size=20`: search (prefix/fuzzy)
- `GET /v1/suggest?q=...&limit=10`: autocomplete suggestions
- `GET /v1/health`: liveness check
- `GET /v1/ready`: readiness (DB ping, 1s timeout) plus which optional features are enabled
- `POST /v1/analytics/events`, `POST /v1/feedback`, `POST /v1/feedback/upload-image`: app telemetry / feedback
- `POST /v1/llm/ark`: Volcengine Ark Responses API proxy (JSON or SSE)
- `POST /v1/tts`: Volcengine Doubao text-to-speech (mp3)

All JSON responses use the envelope `{data, error, code, request_id, server_time}`; `code` is an
optional stable machine-readable error code (`invalid_request`, `invalid_body`, `body_too_large`,
`not_found`, `method_not_allowed`, `rate_limited`, `service_unavailable`, `busy`, `budget_exceeded`,
`upstream_error`, `internal_error`). Unknown routes return a JSON 404 and wrong methods a JSON 405.
Internal errors never include details (they are logged with the `request_id`).

## Project Structure

- `cmd/api`: API server entrypoint
- `cmd/importer`: ECDICT CSV importer entrypoint
- `internal/config`: environment loading/validation
- `internal/db`: SQLite bootstrap and schema setup
- `internal/repo`: dictionary data access layer
- `internal/httpapi`: routing, middleware (request id, access log, panic recovery) and handlers
- `internal/ratelimit`, `internal/llmproxy`, `internal/tts`, `internal/upload`, `internal/feedback`, `internal/analytics`: feature services
- `migrations/schema.sql`: DB schema and indexes (embedded into the binaries)
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

Other targets: `make test` (race-enabled tests), `make vet`, `make build` (writes `./bin/api`),
`make run-local` (loads `./.env`).

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
# the container runs as uid 10001; the bind-mounted data dir must be writable by it
sudo chown -R 10001:10001 ./data
docker compose up -d api
```

The compose file publishes the API on `127.0.0.1:8080` only; put a reverse proxy (Caddy) in front.
The image has a `HEALTHCHECK` (`api -healthcheck`, which GETs `/v1/health` locally).

5. Verify

```bash
curl 'http://127.0.0.1:8080/v1/health'
```

## Environment Variables

Configuration is validated at startup; the process exits on malformed numbers, bad CIDRs or a
partially configured feature (e.g. only some `QINIU_*` variables set). Secrets are never logged.

Core:

- `HTTP_ADDR` (default: `:8080`)
- `DB_PATH` (default: `./data/dict.db`, container default: `/app/data/dict.db`)
- `SCHEMA_PATH` (optional; the schema is embedded in the binary, set this only to override it)
- `TRUSTED_PROXIES`: comma-separated CIDRs/IPs whose `X-Forwarded-For` is honoured (default: loopback,
  RFC1918 and `fc00::/7`). When the direct peer is trusted, the client IP is the right-most
  `X-Forwarded-For` hop that is not itself trusted; otherwise it is the peer address. Rate limits key on this IP.

Per-client-IP rate limits (requests per minute, `0` disables; over the limit → `429` + `Retry-After`):

- `LLM_RATE_PER_MINUTE` (default `60`), `TTS_RATE_PER_MINUTE` (`120`), `ANALYTICS_RATE_PER_MINUTE` (`60`),
  `FEEDBACK_RATE_PER_MINUTE` (`10`), `UPLOAD_RATE_PER_MINUTE` (`5`), `SEARCH_RATE_PER_MINUTE` (`120`, shared by `/v1/search` and `/v1/suggest`)

LLM proxy (`POST /v1/llm/ark`):

- `LLM_ARK_API_KEY`: enables the proxy; the key is injected server-side.
- `LLM_ARK_MODELS`: comma-separated model allowlist (default matches the iOS app); other models → `400`.
- The request body is **not** forwarded verbatim: only `model`, `input`, `stream`, `thinking`, `text`,
  `temperature` and `max_output_tokens` are kept (everything else, e.g. `tools`, `previous_response_id`,
  is dropped), `input` is required, `max_output_tokens` is clamped to 4500 (default 4500) and
  `"store": false` is always sent so Ark does not retain responses. JSON/SSE responses are streamed back unchanged.
- `LLM_MAX_BODY_MB` (default `6`), `LLM_TIMEOUT_SECONDS` (default `180`)
- `LLM_MAX_CONCURRENT` (default `32`): global in-flight cap; when full → `503` + `Retry-After`.
- `LLM_DAILY_REQUEST_BUDGET` (default `0` = unlimited): global requests per UTC day; when exhausted → `503` + `Retry-After` (seconds until UTC midnight). In-memory, resets on restart.

TTS (`POST /v1/tts`):

- `VOLC_SPEECH_API_KEY`: enables TTS (Volcengine Doubao `seed-tts-2.0`). Body `{"text","speaker","speech_rate"}`, returns `audio/mpeg`.
- `VOLC_TTS_SPEAKERS` (speaker allowlist, default matches the iOS app), `VOLC_TTS_RESOURCE_ID` (default `seed-tts-2.0`)
- `TTS_CACHE_DIR` (default `<DB_PATH dir>/tts-cache`): texts up to 100 characters are cached; longer texts are never stored.
- `TTS_CACHE_MAX_MB` (default `2048`, `0` = no cap): an hourly sweep deletes the least recently used files beyond the cap.
- Upstream calls are detached from the client connection (30s timeout) so a disconnect does not waste a paid call; identical concurrent requests share one upstream call.

Feedback / upload (all-or-nothing groups):

- `FEISHU_APP_ID`, `FEISHU_APP_SECRET`, `FEISHU_BITABLE_APP_TOKEN`, `FEISHU_BITABLE_TABLE_ID`: enable `POST /v1/feedback`.
  Submissions are deduplicated by `client_feedback_id` (a retried id is acknowledged without a second Feishu record).
  Field limits (characters): content 4000, category 64, user_id 128, device 128, ios_version/app_version/locale 32,
  client_feedback_id 128, screenshot_url 512. `screenshot_url` must start with `QINIU_PUBLIC_BASE_URL + "/feedback/"`
  (or be `https://` when Qiniu is not configured).
- `QINIU_ACCESS_KEY`, `QINIU_SECRET_KEY`, `QINIU_BUCKET`, `QINIU_UPLOAD_URL`, `QINIU_PUBLIC_BASE_URL`: enable
  `POST /v1/feedback/upload-image` (max 10 MB). The image type is detected from the file bytes (JPEG, PNG, WebP,
  HEIC/HEIF; anything else → `400`), the client `Content-Type` is ignored. `key_prefix` must match
  `^feedback/[a-z0-9_-]{1,32}$`, otherwise `feedback/other` is used. Upload tokens live 300s and carry
  `fsizeLimit`/`mimeLimit`/`detectMime`.

Analytics:

- `ANALYTICS_RETENTION_DAYS` (default `395`, `0` disables): a daily job deletes older events.
- Invalid events in a batch (missing fields, over-long fields, `event_time_ms` outside `[now-30d, now+1d]`) are
  skipped; the response is `{"accepted": N, "rejected": M}`.

Request body limits: analytics 256 KB, feedback 32 KB, TTS 16 KB, LLM `LLM_MAX_BODY_MB`; oversized bodies → `413`.

## API Examples

```bash
curl 'http://127.0.0.1:8080/v1/word/apple'
curl 'http://127.0.0.1:8080/v1/suggest?q=app&limit=5'
curl 'http://127.0.0.1:8080/v1/search?q=apple&mode=prefix&page=1&page_size=20'
curl 'http://127.0.0.1:8080/v1/search?q=network security&mode=fuzzy&page=1&page_size=20'
curl -N -X POST 'http://127.0.0.1:8080/v1/llm/ark' -H 'Content-Type: application/json' \
  -d '{"model":"doubao-seed-2-1-lite-260915","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}'
curl -X POST 'http://127.0.0.1:8080/v1/tts' -H 'Content-Type: application/json' \
  -d '{"text":"serendipity","speaker":"en_female_dacey_uranus_bigtts","speech_rate":0}' -o out.mp3
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
