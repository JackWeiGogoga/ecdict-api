package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	maxBatchSize      = 100
	maxEventNameLen   = 80
	maxPageNameLen    = 40
	maxPlatformLen    = 20
	maxVersionLen     = 32
	maxLanguageLen    = 32
	maxParamsJSONSize = 16 << 10
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
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

func (s *Service) InsertBatch(ctx context.Context, events []Event) (int, error) {
	if len(events) == 0 {
		return 0, fmt.Errorf("missing events")
	}
	if len(events) > maxBatchSize {
		return 0, fmt.Errorf("too many events")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
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
		return 0, err
	}
	defer stmt.Close()

	nowMS := time.Now().UTC().UnixMilli()
	accepted := 0

	for i := range events {
		event, paramsJSON, err := normalizeEvent(events[i])
		if err != nil {
			return 0, fmt.Errorf("event[%d]: %w", i, err)
		}

		result, err := stmt.ExecContext(
			ctx,
			event.EventID,
			event.EventName,
			event.UserID,
			event.SessionID,
			event.Platform,
			event.AppVersion,
			event.Build,
			event.SystemLanguage,
			event.SystemLocale,
			event.AppLanguage,
			event.PageName,
			event.EventTimeMS,
			event.DurationMS,
			paramsJSON,
			nowMS,
			nowMS,
		)
		if err != nil {
			return 0, err
		}

		rowsAffected, err := result.RowsAffected()
		if err == nil && rowsAffected > 0 {
			accepted += int(rowsAffected)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return accepted, nil
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
	if len(event.EventName) > maxEventNameLen {
		return Event{}, "", fmt.Errorf("event_name too long")
	}
	if len(event.PageName) > maxPageNameLen {
		return Event{}, "", fmt.Errorf("page_name too long")
	}
	if len(event.Platform) > maxPlatformLen {
		return Event{}, "", fmt.Errorf("platform too long")
	}
	if len(event.AppVersion) > maxVersionLen || len(event.Build) > maxVersionLen {
		return Event{}, "", fmt.Errorf("version field too long")
	}
	if len(event.SystemLanguage) > maxLanguageLen || len(event.SystemLocale) > maxLanguageLen || len(event.AppLanguage) > maxLanguageLen {
		return Event{}, "", fmt.Errorf("language field too long")
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
