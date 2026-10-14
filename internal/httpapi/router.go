// Package httpapi wires the public HTTP API: routing, middleware and handlers.
package httpapi

import (
	"database/sql"
	"log/slog"
	"net/http"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/config"
	"gogoga_dictionary/internal/feedback"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/ratelimit"
	"gogoga_dictionary/internal/repo"
	"gogoga_dictionary/internal/tts"
	"gogoga_dictionary/internal/upload"
)

// Deps are the services behind the API. Optional features (Feedback, Upload, LLM, TTS) may be nil,
// in which case their endpoints answer 503.
type Deps struct {
	DB        *sql.DB
	Words     *repo.WordRepository
	Analytics *analytics.Service
	Feedback  *feedback.Service
	Upload    *upload.Service
	LLM       *llmproxy.Proxy
	TTS       *tts.Service

	// QiniuPublicBaseURL constrains feedback screenshot_url values (see feedback.Record.Normalize).
	QiniuPublicBaseURL string
	RateLimits         config.RateLimits
	Resolver           *ratelimit.IPResolver
	Logger             *slog.Logger
}

type Handler struct {
	Deps
}

// NewRouter returns the full handler chain: request id -> access log -> panic recovery -> routes.
func NewRouter(d Deps) http.Handler {
	if d.Resolver == nil {
		d.Resolver, _ = ratelimit.NewIPResolver("")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	h := &Handler{Deps: d}

	limit := func(perMinute int, fn http.HandlerFunc) http.Handler {
		return ratelimit.Middleware(ratelimit.New(perMinute, d.Resolver), fn)
	}
	searchLimiter := ratelimit.New(d.RateLimits.Search, d.Resolver) // shared by search + suggest

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", h.health)
	mux.HandleFunc("GET /v1/ready", h.ready)
	// {word...} also matches the empty remainder so GET /v1/word/ keeps answering 400 "missing word",
	// and words containing "/" keep working as before.
	mux.HandleFunc("GET /v1/word/{word...}", h.getWord)
	mux.Handle("GET /v1/search", ratelimit.Middleware(searchLimiter, http.HandlerFunc(h.search)))
	mux.Handle("GET /v1/suggest", ratelimit.Middleware(searchLimiter, http.HandlerFunc(h.suggest)))
	mux.Handle("POST /v1/analytics/events", limit(d.RateLimits.Analytics, h.submitAnalyticsEvents))
	mux.Handle("POST /v1/feedback", limit(d.RateLimits.Feedback, h.submitFeedback))
	mux.Handle("POST /v1/feedback/upload-image", limit(d.RateLimits.Upload, h.uploadFeedbackImage))
	mux.Handle("POST /v1/llm/ark", limit(d.RateLimits.LLM, h.proxyLLM))
	mux.Handle("POST /v1/tts", limit(d.RateLimits.TTS, h.synthesizeSpeech))

	return requestIDMiddleware(accessLogMiddleware(d.Logger, d.Resolver, recoverMiddleware(jsonFallback(mux))))
}
