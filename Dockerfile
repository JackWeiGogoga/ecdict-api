FROM golang:1.22-bookworm AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -tags "sqlite_fts5" -o /out/api ./cmd/api
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -tags "sqlite_fts5" -o /out/importer ./cmd/importer

FROM debian:bookworm-slim AS runtime
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/api /usr/local/bin/api
COPY --from=builder /out/importer /usr/local/bin/importer
COPY migrations ./migrations

RUN mkdir -p /app/data /app/datasets

ENV HTTP_ADDR=:8080
ENV DB_PATH=/app/data/dict.db
ENV SCHEMA_PATH=/app/migrations/schema.sql

EXPOSE 8080

CMD ["api"]
