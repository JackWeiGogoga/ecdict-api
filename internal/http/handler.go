package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/feedback"
	"gogoga_dictionary/internal/repo"
)

type Handler struct {
	repo        *repo.WordRepository
	feedbackSvc *feedback.Service
}

func NewHandler(repo *repo.WordRepository, feedbackSvc *feedback.Service) *Handler {
	return &Handler{
		repo:        repo,
		feedbackSvc: feedbackSvc,
	}
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
	mux.HandleFunc("/v1/feedback", h.submitFeedback)
}

type feedbackRequest struct {
	ClientFeedbackID string `json:"client_feedback_id"`
	Content          string `json:"content"`
	UserID           string `json:"user_id"`
	Device           string `json:"device"`
	IOSVersion       string `json:"ios_version"`
	AppVersion       string `json:"app_version"`
	Locale           string `json:"locale"`
	ScreenshotURL    string `json:"screenshot_url"`
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

func (h *Handler) submitFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, response{Error: "method not allowed", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}
	if h.feedbackSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, response{Error: "feedback service unavailable", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{Error: "invalid json body", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	req.ClientFeedbackID = strings.TrimSpace(req.ClientFeedbackID)
	req.Content = strings.TrimSpace(req.Content)
	req.UserID = strings.TrimSpace(req.UserID)
	req.Device = strings.TrimSpace(req.Device)
	req.IOSVersion = strings.TrimSpace(req.IOSVersion)
	req.AppVersion = strings.TrimSpace(req.AppVersion)
	req.Locale = strings.TrimSpace(req.Locale)
	req.ScreenshotURL = strings.TrimSpace(req.ScreenshotURL)

	if req.ClientFeedbackID == "" {
		writeJSON(w, http.StatusBadRequest, response{Error: "missing client_feedback_id", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}
	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, response{Error: "missing content", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}
	if len([]rune(req.Content)) > 4000 {
		writeJSON(w, http.StatusBadRequest, response{Error: "content too long", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	err := h.feedbackSvc.Submit(r.Context(), feedback.Record{
		ClientFeedbackID: req.ClientFeedbackID,
		Content:          req.Content,
		UserID:           req.UserID,
		Device:           req.Device,
		IOSVersion:       req.IOSVersion,
		AppVersion:       req.AppVersion,
		Locale:           req.Locale,
		ScreenshotURL:    req.ScreenshotURL,
		CreatedAt:        time.Now().UTC(),
	})
	if err != nil {
		log.Printf("submit feedback failed: %v", err)
		writeJSON(w, http.StatusBadGateway, response{Error: "submit feedback failed", RequestID: requestID(r), ServerTime: nowISO()})
		return
	}

	writeJSON(w, http.StatusOK, response{
		Data: map[string]any{
			"accepted": true,
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
