package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"gogoga_dictionary/migrations"
)

// MaxOpenConns bounds concurrent SQLite connections; WAL allows parallel readers and one writer.
const MaxOpenConns = 8

// OpenSQLite opens path with WAL, synchronous=NORMAL (safe with WAL) and a 5s busy timeout.
// The pragmas are DSN parameters so every pooled connection gets them.
func OpenSQLite(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(MaxOpenConns)
	db.SetMaxIdleConns(MaxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ApplySchema executes the embedded schema, or the file at schemaPath when it is non-empty
// (kept for compatibility with SCHEMA_PATH / -schema).
func ApplySchema(ctx context.Context, conn *sql.DB, schemaPath string) error {
	schema := migrations.Schema
	if strings.TrimSpace(schemaPath) != "" {
		b, err := os.ReadFile(schemaPath)
		if err != nil {
			return err
		}
		schema = string(b)
	}
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema failed: %w", err)
	}
	return nil
}
