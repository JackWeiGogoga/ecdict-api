package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/db"
)

func main() {
	var (
		csvPath    string
		dbPath     string
		schemaPath string
		batchSize  int
	)

	flag.StringVar(&csvPath, "csv", "", "path to ECDICT csv file")
	flag.StringVar(&dbPath, "db", "./data/dict.db", "sqlite db path")
	flag.StringVar(&schemaPath, "schema", "./migrations/schema.sql", "schema file path")
	flag.IntVar(&batchSize, "batch", 1000, "rows per transaction commit")
	flag.Parse()

	if strings.TrimSpace(csvPath) == "" {
		log.Fatal("missing -csv file path")
	}

	if err := os.MkdirAll("./data", 0o755); err != nil {
		log.Fatalf("create data dir failed: %v", err)
	}

	conn, err := db.OpenSQLite(dbPath)
	if err != nil {
		log.Fatalf("open db failed: %v", err)
	}
	defer conn.Close()

	if err := db.ApplySchema(context.Background(), conn, schemaPath); err != nil {
		log.Fatalf("apply schema failed: %v", err)
	}

	if err := importCSV(conn, csvPath, batchSize); err != nil {
		log.Fatalf("import csv failed: %v", err)
	}
}

func importCSV(conn *sql.DB, csvPath string, batchSize int) error {
	f, err := os.Open(csvPath)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read csv header failed: %w", err)
	}

	index := make(map[string]int, len(header))
	for i, key := range header {
		index[strings.ToLower(strings.TrimSpace(key))] = i
	}

	start := time.Now()
	total := 0

	tx, stmt, err := beginUpsert(conn)
	if err != nil {
		return err
	}

	commit := func() error {
		if err := stmt.Close(); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}

	rollback := func() {
		_ = stmt.Close()
		_ = tx.Rollback()
	}

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			rollback()
			return err
		}

		word := get(row, index, "word")
		if strings.TrimSpace(word) == "" {
			continue
		}

		_, err = stmt.Exec(
			word,
			strings.ToLower(word),
			get(row, index, "phonetic"),
			get(row, index, "definition"),
			get(row, index, "translation"),
			get(row, index, "pos"),
			parseInt(get(row, index, "collins")),
			parseInt(get(row, index, "oxford")),
			get(row, index, "tag"),
			parseInt(get(row, index, "bnc")),
			parseInt(get(row, index, "frq")),
			get(row, index, "exchange"),
			get(row, index, "detail"),
			get(row, index, "audio"),
		)
		if err != nil {
			rollback()
			return err
		}

		total++
		if total%batchSize == 0 {
			if err := commit(); err != nil {
				return err
			}
			tx, stmt, err = beginUpsert(conn)
			if err != nil {
				return err
			}
			log.Printf("imported %d rows", total)
		}
	}

	if err := commit(); err != nil {
		return err
	}

	log.Printf("import completed: %d rows in %s", total, time.Since(start))
	return nil
}

func beginUpsert(conn *sql.DB) (*sql.Tx, *sql.Stmt, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, nil, err
	}

	stmt, err := tx.Prepare(`
INSERT INTO words (
    word, word_lower, phonetic, definition, translation, pos,
    collins, oxford, tag, bnc, frq, exchange, detail, audio
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(word_lower) DO UPDATE SET
    word = excluded.word,
    phonetic = excluded.phonetic,
    definition = excluded.definition,
    translation = excluded.translation,
    pos = excluded.pos,
    collins = excluded.collins,
    oxford = excluded.oxford,
    tag = excluded.tag,
    bnc = excluded.bnc,
    frq = excluded.frq,
    exchange = excluded.exchange,
    detail = excluded.detail,
    audio = excluded.audio;
`)
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	return tx, stmt, nil
}

func get(row []string, index map[string]int, key string) string {
	i, ok := index[key]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func parseInt(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}
