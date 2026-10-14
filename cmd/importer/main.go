package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/repo"
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
	flag.StringVar(&schemaPath, "schema", "", "schema file path (default: embedded migrations/schema.sql)")
	flag.IntVar(&batchSize, "batch", 1000, "rows per transaction commit")
	flag.Parse()

	if strings.TrimSpace(csvPath) == "" {
		log.Fatal("missing -csv file path")
	}
	if batchSize <= 0 {
		batchSize = 1000
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		log.Fatalf("create data dir failed: %v", err)
	}

	conn, err := db.OpenSQLite(dbPath)
	if err != nil {
		log.Fatalf("open db failed: %v", err)
	}
	defer conn.Close()

	ctx := context.Background()
	if err := db.ApplySchema(ctx, conn, schemaPath); err != nil {
		log.Fatalf("apply schema failed: %v", err)
	}

	if err := importCSV(ctx, repo.NewWordRepository(conn), csvPath, batchSize); err != nil {
		log.Fatalf("import csv failed: %v", err)
	}
}

func importCSV(ctx context.Context, words *repo.WordRepository, csvPath string, batchSize int) error {
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
	batch := make([]repo.Word, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := words.UpsertWords(ctx, batch); err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		log.Printf("imported %d rows", total)
		return nil
	}

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		word := get(row, index, "word")
		if word == "" {
			continue
		}
		batch = append(batch, repo.Word{
			Word:        word,
			Phonetic:    get(row, index, "phonetic"),
			Definition:  get(row, index, "definition"),
			Translation: get(row, index, "translation"),
			Pos:         get(row, index, "pos"),
			Collins:     parseInt(get(row, index, "collins")),
			Oxford:      parseInt(get(row, index, "oxford")),
			Tag:         get(row, index, "tag"),
			BNC:         parseInt(get(row, index, "bnc")),
			FRQ:         parseInt(get(row, index, "frq")),
			Exchange:    get(row, index, "exchange"),
			Detail:      get(row, index, "detail"),
			Audio:       get(row, index, "audio"),
		})
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	log.Printf("import completed: %d rows in %s", total, time.Since(start))
	return nil
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
