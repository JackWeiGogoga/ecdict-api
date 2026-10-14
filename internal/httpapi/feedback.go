package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gogoga_dictionary/internal/apperr"
	"gogoga_dictionary/internal/feedback"
	"gogoga_dictionary/internal/upload"
)

const (
	maxFeedbackBodyBytes = 32 << 10
	// Multipart overhead allowance on top of the image itself.
	maxUploadRequestBytes = upload.MaxImageBytes + 1<<20
	uploadReadTimeout     = 90 * time.Second
)

type feedbackRequest struct {
	ClientFeedbackID string `json:"client_feedback_id"`
	Content          string `json:"content"`
	UserID           string `json:"user_id"`
	Device           string `json:"device"`
	IOSVersion       string `json:"ios_version"`
	AppVersion       string `json:"app_version"`
	Locale           string `json:"locale"`
	Category         string `json:"category"`
	ScreenshotURL    string `json:"screenshot_url"`
}

func (h *Handler) submitFeedback(w http.ResponseWriter, r *http.Request) {
	if h.Feedback == nil {
		unavailable(w, r, "feedback service unavailable")
		return
	}
	req, err := decodeJSON[feedbackRequest](w, r, maxFeedbackBodyBytes)
	if err != nil {
		failDecode(w, r, err)
		return
	}

	rec := feedback.Record{
		ClientFeedbackID: req.ClientFeedbackID,
		Content:          req.Content,
		Category:         req.Category,
		UserID:           req.UserID,
		Device:           req.Device,
		IOSVersion:       req.IOSVersion,
		AppVersion:       req.AppVersion,
		Locale:           req.Locale,
		ScreenshotURL:    req.ScreenshotURL,
		CreatedAt:        time.Now().UTC(),
	}
	if err := rec.Normalize(h.QiniuPublicBaseURL); err != nil {
		failErr(w, r, err, "invalid feedback")
		return
	}

	duplicate, err := h.Feedback.Submit(r.Context(), rec)
	if err != nil {
		slog.Error("submit feedback failed", "err", err, "request_id", requestIDFrom(r))
		fail(w, r, http.StatusBadGateway, codeUpstream, "submit feedback failed")
		return
	}
	if duplicate {
		slog.Info("duplicate feedback ignored", "request_id", requestIDFrom(r))
	}
	ok(w, r, map[string]any{"accepted": true})
}

func (h *Handler) uploadFeedbackImage(w http.ResponseWriter, r *http.Request) {
	if h.Upload == nil {
		unavailable(w, r, "upload service unavailable")
		return
	}
	// Slow mobile uploads need more than the server-wide 30s read/write timeouts; the Qiniu call
	// (25s client timeout) happens after the body is read.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(uploadReadTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(uploadReadTimeout + 30*time.Second))

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "file too large")
			return
		}
		fail(w, r, http.StatusBadRequest, codeInvalidBody, "invalid multipart form or file too large")
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, _, err := r.FormFile("file")
	if err != nil {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "missing file")
		return
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, upload.MaxImageBytes+1))
	if err != nil {
		fail(w, r, http.StatusBadRequest, codeInvalidBody, "read file failed")
		return
	}
	if len(content) > upload.MaxImageBytes {
		fail(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "file too large")
		return
	}

	// The client Content-Type and filename are ignored; the type is sniffed from the bytes.
	result, err := h.Upload.UploadImage(r.Context(), content, r.FormValue("key_prefix"))
	if err != nil {
		if _, isValidation := apperr.AsValidation(err); isValidation {
			failErr(w, r, err, "")
			return
		}
		slog.Error("upload feedback image failed", "err", err, "request_id", requestIDFrom(r))
		fail(w, r, http.StatusBadGateway, codeUpstream, "upload image failed")
		return
	}
	ok(w, r, map[string]any{"url": result.URL, "key": result.Key})
}
