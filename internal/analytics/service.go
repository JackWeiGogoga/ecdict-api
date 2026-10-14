package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"gogoga_dictionary/internal/apperr"
)

const (
	MaxBatchSize      = 100
	maxIDLen          = 128
	maxEventNameLen   = 64
	maxPageNameLen    = 64
	maxShortFieldLen  = 32
	maxParamsJSONSize = 16 << 10
	maxEventAge       = 30 * 24 * time.Hour
	maxEventFuture    = 24 * time.Hour
)

type Event struct {
	EventID        string
	EventName      string
	UserID         string
	SessionID      string
	Platform       string
	AppVersion     string
	Build          string
	SystemLanguage string
	SystemLocale   string
	AppLanguage    string
	PageName       string
	EventTimeMS    int64
	DurationMS     *int64
	Params         map[string]any
}

type Service struct {
	db  *sql.DB
	now func() time.Time
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// Result counts inserted events and events skipped as invalid. Duplicates (same event_id) are neither.
type Result struct {
	Accepted int
	Rejected int
}

// InsertBatch stores valid events and skips invalid ones; only an empty/oversized batch is a client
// error (*apperr.Validation). Any other error is internal.
func (s *Service) InsertBatch(ctx context.Context, events []Event) (Result, error) {
	if len(events) == 0 {
		return Result{}, apperr.Invalid("missing events")
	}
	if len(events) > MaxBatchSize {
		return Result{}, apperr.Invalid("too many events")
	}

	now := s.now().UTC()
	minTime, maxTime := now.Add(-maxEventAge).UnixMilli(), now.Add(maxEventFuture).UnixMilli()

	var res Result
	type row struct {
		event  Event
		params string
	}
	valid := make([]row, 0, len(events))
	for i := range events {
		event, paramsJSON, err := normalizeEvent(events[i])
		if err == nil && (event.EventTimeMS < minTime || event.EventTimeMS > maxTime) {
			err = fmt.Errorf("event_time_ms out of range")
		}
		if err != nil {
			res.Rejected++
			continue
		}
		valid = append(valid, row{event, paramsJSON})
	}
	if len(valid) == 0 {
		return res, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO analytics_events (
    event_id, event_name, user_id, session_id, platform, app_version, build,
    system_language, system_locale, app_language, page_name,
    event_time_ms, duration_ms, params_json, server_time_ms, created_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_id) DO NOTHING
`)
	if err != nil {
		return Result{}, err
	}
	defer stmt.Close()

	nowMS := now.UnixMilli()
	for _, v := range valid {
		e := v.event
		result, err := stmt.ExecContext(ctx,
			e.EventID, e.EventName, e.UserID, e.SessionID, e.Platform, e.AppVersion, e.Build,
			e.SystemLanguage, e.SystemLocale, e.AppLanguage, e.PageName,
			e.EventTimeMS, e.DurationMS, v.params, nowMS, nowMS,
		)
		if err != nil {
			return Result{}, err
		}
		if n, err := result.RowsAffected(); err == nil && n > 0 {
			res.Accepted += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return res, nil
}

// PurgeOlderThan deletes events created before cutoff in batches, returning the number removed.
func (s *Service) PurgeOlderThan(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `
DELETE FROM analytics_events
WHERE id IN (SELECT id FROM analytics_events WHERE created_at_ms < ? LIMIT ?)`,
			cutoff.UnixMilli(), batchSize)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batchSize) {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(50 * time.Millisecond): // let writers in between batches
		}
	}
}

// RunRetention purges events older than retentionDays once at start (after a short delay) and then daily,
// until ctx is cancelled. retentionDays <= 0 disables it.
func (s *Service) RunRetention(ctx context.Context, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		cutoff := s.now().Add(-time.Duration(retentionDays) * 24 * time.Hour)
		n, err := s.PurgeOlderThan(ctx, cutoff, 5000)
		if err != nil && ctx.Err() == nil {
			slog.Error("analytics retention failed", "err", err)
		} else if n > 0 {
			slog.Info("analytics retention purged events", "deleted", n, "retention_days", retentionDays)
		}
		timer.Reset(24 * time.Hour)
	}
}

func normalizeEvent(event Event) (Event, string, error) {
	event.EventID = strings.TrimSpace(event.EventID)
	event.EventName = strings.TrimSpace(event.EventName)
	event.UserID = strings.TrimSpace(event.UserID)
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.Platform = strings.TrimSpace(event.Platform)
	event.AppVersion = strings.TrimSpace(event.AppVersion)
	event.Build = strings.TrimSpace(event.Build)
	event.SystemLanguage = strings.TrimSpace(event.SystemLanguage)
	event.SystemLocale = strings.TrimSpace(event.SystemLocale)
	event.AppLanguage = strings.TrimSpace(event.AppLanguage)
	event.PageName = strings.TrimSpace(event.PageName)

	if event.EventID == "" {
		return Event{}, "", fmt.Errorf("missing event_id")
	}
	if event.EventName == "" {
		return Event{}, "", fmt.Errorf("missing event_name")
	}
	if event.UserID == "" {
		return Event{}, "", fmt.Errorf("missing user_id")
	}
	if event.SessionID == "" {
		return Event{}, "", fmt.Errorf("missing session_id")
	}
	if event.Platform == "" {
		return Event{}, "", fmt.Errorf("missing platform")
	}
	if event.AppVersion == "" {
		return Event{}, "", fmt.Errorf("missing app_version")
	}
	if event.Build == "" {
		return Event{}, "", fmt.Errorf("missing build")
	}
	if event.SystemLanguage == "" {
		return Event{}, "", fmt.Errorf("missing system_language")
	}
	if event.SystemLocale == "" {
		return Event{}, "", fmt.Errorf("missing system_locale")
	}
	if event.AppLanguage == "" {
		return Event{}, "", fmt.Errorf("missing app_language")
	}
	if event.EventTimeMS <= 0 {
		return Event{}, "", fmt.Errorf("invalid event_time_ms")
	}
	limits := []struct {
		name string
		v    string
		max  int
	}{
		{"event_id", event.EventID, maxIDLen},
		{"user_id", event.UserID, maxIDLen},
		{"session_id", event.SessionID, maxIDLen},
		{"event_name", event.EventName, maxEventNameLen},
		{"page_name", event.PageName, maxPageNameLen},
		{"platform", event.Platform, maxShortFieldLen},
		{"app_version", event.AppVersion, maxShortFieldLen},
		{"build", event.Build, maxShortFieldLen},
		{"system_language", event.SystemLanguage, maxShortFieldLen},
		{"system_locale", event.SystemLocale, maxShortFieldLen},
		{"app_language", event.AppLanguage, maxShortFieldLen},
	}
	for _, l := range limits {
		if utf8.RuneCountInString(l.v) > l.max {
			return Event{}, "", fmt.Errorf("%s too long", l.name)
		}
	}
	if event.DurationMS != nil && *event.DurationMS < 0 {
		return Event{}, "", fmt.Errorf("invalid duration_ms")
	}

	if event.Params == nil {
		event.Params = map[string]any{}
	}
	paramsJSONBytes, err := json.Marshal(event.Params)
	if err != nil {
		return Event{}, "", fmt.Errorf("marshal params failed: %w", err)
	}
	if len(paramsJSONBytes) > maxParamsJSONSize {
		return Event{}, "", fmt.Errorf("params too large")
	}

	return event, string(paramsJSONBytes), nil
}
