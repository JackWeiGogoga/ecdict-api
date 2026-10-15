package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/asr"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/ratelimit"
	"gogoga_dictionary/internal/tts"
)

const (
	maxTTSBodyBytes = 16 << 10
	llmReadTimeout  = 90 * time.Second
	ttsWriteTimeout = 60 * time.Second
	asrTimeout      = 45 * time.Second
	busyRetryAfter  = 2 * time.Second
)

func (h *Handler) proxyLLM(w http.ResponseWriter, r *http.Request) {
	if h.LLM == nil {
		unavailable(w, r, "llm proxy unavailable")
		return
	}
	// The server-wide WriteTimeout (30s) is too short for long SSE answers: extend it per request.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(llmReadTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(h.LLM.Timeout() + 10*time.Second))

	err := h.LLM.Forward(w, r)
	if err == nil {
		return
	}

	switch {
	case errors.Is(err, llmproxy.ErrBusy):
		w.Header().Set("Retry-After", ratelimit.RetryAfterSeconds(busyRetryAfter))
		fail(w, r, http.StatusServiceUnavailable, codeBusy, "llm proxy busy")
	case errors.Is(err, llmproxy.ErrBudgetExceeded):
		w.Header().Set("Retry-After", ratelimit.RetryAfterSeconds(h.LLM.BudgetResetIn()))
		fail(w, r, http.StatusServiceUnavailable, codeBudgetExceeded, "llm daily budget exceeded")
	case errors.Is(err, llmproxy.ErrBodyTooLarge):
		fail(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body too large")
	case errors.Is(err, llmproxy.ErrInvalidBody):
		fail(w, r, http.StatusBadRequest, codeInvalidBody, "invalid json body")
	case errors.Is(err, llmproxy.ErrMissingInput):
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "missing input")
	case errors.Is(err, llmproxy.ErrModelNotAllowed):
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, "model not allowed")
	default:
		slog.Error("llm proxy failed", "err", err, "request_id", requestIDFrom(r))
		fail(w, r, http.StatusBadGateway, codeUpstream, "llm upstream request failed")
	}
}

type ttsRequest struct {
	Text       string `json:"text"`
	Speaker    string `json:"speaker"`
	SpeechRate int    `json:"speech_rate"`
}

// synthesizeSpeech returns an mp3 (Content-Type audio/mpeg); errors are the usual JSON envelope.
func (h *Handler) synthesizeSpeech(w http.ResponseWriter, r *http.Request) {
	if h.TTS == nil {
		unavailable(w, r, "tts service unavailable")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(ttsWriteTimeout))

	req, err := decodeJSON[ttsRequest](w, r, maxTTSBodyBytes)
	if err != nil {
		failDecode(w, r, err)
		return
	}

	audio, cached, err := h.TTS.Synthesize(r.Context(), tts.Request{
		Text:       req.Text,
		Speaker:    strings.TrimSpace(req.Speaker),
		SpeechRate: req.SpeechRate,
	})
	if err != nil {
		if errors.Is(err, tts.ErrInvalidText) || errors.Is(err, tts.ErrSpeakerNotAllowed) {
			failErr(w, r, err, "")
			return
		}
		slog.Error("tts failed", "err", err, "request_id", requestIDFrom(r))
		fail(w, r, http.StatusBadGateway, codeUpstream, "tts upstream failed")
		return
	}

	cacheStatus := "miss"
	if cached {
		cacheStatus = "hit"
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.Header().Set("X-TTS-Cache", cacheStatus)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// recognizeSpeech takes a raw WAV body (Content-Type audio/wav) and returns {"data":{"text":"..."}}.
// The audio is only forwarded upstream: never written to disk or logged.
func (h *Handler) recognizeSpeech(w http.ResponseWriter, r *http.Request) {
	if h.ASR == nil {
		unavailable(w, r, "asr service unavailable")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(asrTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(asrTimeout + 10*time.Second))

	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, asr.MaxAudioBytes))
	if err != nil {
		failDecode(w, r, classifyBodyErr(err))
		return
	}

	text, err := h.ASR.Recognize(r.Context(), audio)
	if err != nil {
		if errors.Is(err, asr.ErrInvalidAudio) {
			failErr(w, r, err, "")
			return
		}
		slog.Error("asr failed", "err", err, "request_id", requestIDFrom(r))
		fail(w, r, http.StatusBadGateway, codeUpstream, "asr upstream failed")
		return
	}
	ok(w, r, map[string]string{"text": text})
}
