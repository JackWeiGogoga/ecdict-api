package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/repo"
)

type Handler struct {
	repo *repo.WordRepository
}

func NewHandler(repo *repo.WordRepository) *Handler {
	return &Handler{repo: repo}
}

type response struct {
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	RequestID  string `json:"request_id"`
	ServerTime string `json:"server_time"`
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", h.health)
	mux.HandleFunc("/v1/word/", h.getWord)
	mux.HandleFunc("/v1/search", h.search)
	mux.HandleFunc("/v1/suggest", h.suggest)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, response{
		Data:       map[string]string{"status": "ok"},
		RequestID:  requestID(r),
		ServerTime: nowISO(),
	})
}

func (h *Handler) getWord(w http.ResponseWriter, r *http.Request) {
	word := strings.TrimPrefix(r.URL.Path, "/v1/word/")
	word = strings.TrimSpace(word)
	if word == "" {
		writeJSON(w, http.StatusBadRequest, response{Error: "missing word", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	item, err := h.repo.GetByWord(r.Context(), word)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{Error: err.Error(), RequestID: requestID(r), ServerTime: nowISO()})
		return
	}
	if item == nil {
		writeJSON(w, http.StatusNotFound, response{Error: "word not found", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	writeJSON(w, http.StatusOK, response{Data: item, RequestID: requestID(r), ServerTime: nowISO()})
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	page, pageSize, err := repo.ValidatePagination(page, pageSize)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{Error: err.Error(), RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	items, total, err := h.repo.Search(r.Context(), q, mode, page, pageSize)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{Error: err.Error(), RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	writeJSON(w, http.StatusOK, response{
		Data: map[string]any{
			"items":     items,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
			"mode":      defaultMode(mode),
			"q":         q,
		},
		RequestID:  requestID(r),
		ServerTime: nowISO(),
	})
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	items, err := h.repo.Suggest(r.Context(), q, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{Error: err.Error(), RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	out := make([]map[string]string, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]string{
			"word":        item.Word,
			"phonetic":    item.Phonetic,
			"translation": item.Translation,
		})
	}

	writeJSON(w, http.StatusOK, response{
		Data: map[string]any{
			"items": out,
			"q":     q,
			"limit": limit,
		},
		RequestID:  requestID(r),
		ServerTime: nowISO(),
	})
}

func writeJSON(w http.ResponseWriter, status int, payload response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func defaultMode(mode string) string {
	if strings.ToLower(mode) == "fuzzy" {
		return "fuzzy"
	}
	return "prefix"
}

func requestID(r *http.Request) string {
	v := r.Header.Get("X-Request-Id")
	if v == "" {
		v = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return v
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}
