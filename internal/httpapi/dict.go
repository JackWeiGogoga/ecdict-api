package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gogoga_dictionary/internal/repo"
)

const maxQueryRunes = 100

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	ok(w, r, map[string]string{"status": "ok"})
}

// ready checks the database and reports which optional features are configured.
func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	features := map[string]bool{
		"analytics": h.Analytics != nil,
		"feedback":  h.Feedback != nil,
		"upload":    h.Upload != nil,
		"llm":       h.LLM != nil,
		"tts":       h.TTS != nil,
	}
	if h.DB == nil || h.DB.PingContext(ctx) != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, envelope{
			Data:  map[string]any{"status": "not_ready", "features": features},
			Error: "database unavailable",
			Code:  codeUnavailable,
		})
		return
	}
	ok(w, r, map[string]any{"status": "ready", "features": features})
}

func (h *Handler) getWord(w http.ResponseWriter, r *http.Request) {
	word := strings.TrimSpace(r.PathValue("word"))
	if word == "" {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "missing word")
		return
	}
	if !utf8.ValidString(word) || utf8.RuneCountInString(word) > maxQueryRunes {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid word")
		return
	}

	item, err := h.Words.GetByWord(r.Context(), word)
	if err != nil {
		failErr(w, r, err, "get word failed")
		return
	}
	if item == nil {
		fail(w, r, http.StatusNotFound, codeNotFound, "word not found")
		return
	}
	ok(w, r, item)
}

// queryParam returns the trimmed query parameter, or false if it is not valid UTF-8 / too long.
func queryParam(r *http.Request, key string) (string, bool) {
	q := strings.TrimSpace(r.URL.Query().Get(key))
	return q, utf8.ValidString(q) && utf8.RuneCountInString(q) <= maxQueryRunes
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q, valid := queryParam(r, "q")
	if !valid {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid q")
		return
	}
	mode := repo.NormalizeMode(r.URL.Query().Get("mode"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	page, pageSize = repo.ValidatePagination(page, pageSize)

	items, total, err := h.Words.Search(r.Context(), q, mode, page, pageSize)
	if err != nil {
		failErr(w, r, err, "search failed")
		return
	}
	ok(w, r, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"mode":      mode,
		"q":         q,
	})
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	q, valid := queryParam(r, "q")
	if !valid {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "invalid q")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	items, err := h.Words.Suggest(r.Context(), q, limit)
	if err != nil {
		failErr(w, r, err, "suggest failed")
		return
	}
	ok(w, r, map[string]any{"items": items, "q": q, "limit": limit})
}
