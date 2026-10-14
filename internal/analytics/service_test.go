package analytics

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogoga_dictionary/internal/apperr"
	"gogoga_dictionary/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.ApplySchema(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	return conn
}

func validEvent(id string, at time.Time) Event {
	return Event{
		EventID: id, EventName: "app_open", UserID: "u", SessionID: "s", Platform: "ios",
		AppVersion: "1.0", Build: "1", SystemLanguage: "en", SystemLocale: "en_US", AppLanguage: "en",
		EventTimeMS: at.UnixMilli(),
	}
}

func TestInsertBatchSkipsInvalid(t *testing.T) {
	svc := NewService(testDB(t))
	now := time.Now()

	tooOld := validEvent("old", now.Add(-31*24*time.Hour))
	future := validEvent("future", now.Add(48*time.Hour))
	longName := validEvent("long", now)
	longName.EventName = strings.Repeat("x", 65)
	longUser := validEvent("longuser", now)
	longUser.UserID = strings.Repeat("u", 129)
	missing := validEvent("", now)

	res, err := svc.InsertBatch(context.Background(), []Event{
		validEvent("a", now), validEvent("b", now.Add(-29*24*time.Hour)), tooOld, future, longName, longUser, missing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Rejected != 5 {
		t.Fatalf("res = %+v", res)
	}
	// Duplicate ids are neither accepted nor rejected.
	res, err = svc.InsertBatch(context.Background(), []Event{validEvent("a", now)})
	if err != nil || res.Accepted != 0 || res.Rejected != 0 {
		t.Fatalf("dup res = %+v err=%v", res, err)
	}
}

func TestInsertBatchValidation(t *testing.T) {
	svc := NewService(testDB(t))
	if _, err := svc.InsertBatch(context.Background(), nil); err == nil {
		t.Fatal("expected error")
	} else if _, ok := apperr.AsValidation(err); !ok {
		t.Fatalf("not validation: %v", err)
	}
	many := make([]Event, MaxBatchSize+1)
	if _, err := svc.InsertBatch(context.Background(), many); err == nil {
		t.Fatal("expected too many events")
	}
}

func TestPurgeOlderThan(t *testing.T) {
	conn := testDB(t)
	svc := NewService(conn)
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		if _, err := svc.InsertBatch(context.Background(), []Event{validEvent(id, now)}); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-400 * 24 * time.Hour).UnixMilli()
	if _, err := conn.Exec(`UPDATE analytics_events SET created_at_ms = ? WHERE event_id IN ('a','b')`, old); err != nil {
		t.Fatal(err)
	}
	n, err := svc.PurgeOlderThan(context.Background(), now.Add(-395*24*time.Hour), 1)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var left int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM analytics_events`).Scan(&left)
	if left != 1 {
		t.Fatalf("left = %d", left)
	}
}
