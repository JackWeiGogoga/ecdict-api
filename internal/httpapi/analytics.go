package httpapi

import (
	"net/http"

	"gogoga_dictionary/internal/analytics"
)

const maxAnalyticsBodyBytes = 256 << 10

type analyticsEventsRequest struct {
	Events []analyticsEventRequest `json:"events"`
}

type analyticsEventRequest struct {
	EventID        string         `json:"event_id"`
	EventName      string         `json:"event_name"`
	UserID         string         `json:"user_id"`
	SessionID      string         `json:"session_id"`
	Platform       string         `json:"platform"`
	AppVersion     string         `json:"app_version"`
	Build          string         `json:"build"`
	SystemLanguage string         `json:"system_language"`
	SystemLocale   string         `json:"system_locale"`
	AppLanguage    string         `json:"app_language"`
	PageName       string         `json:"page_name"`
	EventTimeMS    int64          `json:"event_time_ms"`
	DurationMS     *int64         `json:"duration_ms"`
	Params         map[string]any `json:"params"`
}

// submitAnalyticsEvents stores a batch; invalid events are skipped and counted in "rejected".
func (h *Handler) submitAnalyticsEvents(w http.ResponseWriter, r *http.Request) {
	if h.Analytics == nil {
		unavailable(w, r, "analytics service unavailable")
		return
	}
	req, err := decodeJSON[analyticsEventsRequest](w, r, maxAnalyticsBodyBytes)
	if err != nil {
		failDecode(w, r, err)
		return
	}

	events := make([]analytics.Event, 0, len(req.Events))
	for _, e := range req.Events {
		events = append(events, analytics.Event{
			EventID:        e.EventID,
			EventName:      e.EventName,
			UserID:         e.UserID,
			SessionID:      e.SessionID,
			Platform:       e.Platform,
			AppVersion:     e.AppVersion,
			Build:          e.Build,
			SystemLanguage: e.SystemLanguage,
			SystemLocale:   e.SystemLocale,
			AppLanguage:    e.AppLanguage,
			PageName:       e.PageName,
			EventTimeMS:    e.EventTimeMS,
			DurationMS:     e.DurationMS,
			Params:         e.Params,
		})
	}

	res, err := h.Analytics.InsertBatch(r.Context(), events)
	if err != nil {
		failErr(w, r, err, "insert analytics events failed")
		return
	}
	ok(w, r, map[string]any{"accepted": res.Accepted, "rejected": res.Rejected})
}
