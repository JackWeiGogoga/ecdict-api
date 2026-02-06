package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func OpenSQLite(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func ApplySchema(ctx context.Context, conn *sql.DB, schemaPath string) error {
	b, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, string(b)); err != nil {
		return fmt.Errorf("apply schema failed: %w", err)
	}
	return nil
}
