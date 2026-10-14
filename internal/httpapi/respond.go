package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gogoga_dictionary/internal/apperr"
)

// Stable machine-readable error codes (additive "code" field of the envelope).
const (
	codeInvalidRequest   = "invalid_request"
	codeInvalidBody      = "invalid_body"
	codeBodyTooLarge     = "body_too_large"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeUnavailable      = "service_unavailable"
	codeBusy             = "busy"
	codeBudgetExceeded   = "budget_exceeded"
	codeUpstream         = "upstream_error"
	codeInternal         = "internal_error"
)

// envelope is the response shape the shipped iOS app parses; fields must stay backward compatible.
type envelope struct {
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	Code       string `json:"code,omitempty"`
	RequestID  string `json:"request_id"`
	ServerTime string `json:"server_time"`
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, env envelope) {
	env.RequestID = requestIDFrom(r)
	env.ServerTime = time.Now().UTC().Format(time.RFC3339)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

func ok(w http.ResponseWriter, r *http.Request, data any) {
	writeJSON(w, r, http.StatusOK, envelope{Data: data})
}

func fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeJSON(w, r, status, envelope{Error: msg, Code: code})
}

// failErr renders validation errors as 400 with their message; anything else is logged and
// returned as a generic 500 so internal details never reach clients.
func failErr(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	if v, isValidation := apperr.AsValidation(err); isValidation {
		fail(w, r, http.StatusBadRequest, codeInvalidRequest, v.Msg)
		return
	}
	slog.Error(logMsg, "err", err, "request_id", requestIDFrom(r))
	fail(w, r, http.StatusInternalServerError, codeInternal, "internal error")
}

var (
	errBodyTooLarge = errors.New("request body too large")
	errInvalidJSON  = errors.New("invalid json body")
)

// decodeJSON reads at most limit bytes and decodes a single JSON value. Unknown fields are allowed
// (old clients may send extra keys); trailing data after the value is rejected.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, limit int64) (T, error) {
	var v T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(&v); err != nil {
		return v, classifyBodyErr(err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err != nil {
			if e := classifyBodyErr(err); e == errBodyTooLarge {
				return v, e
			}
		}
		return v, errInvalidJSON
	}
	return v, nil
}

func classifyBodyErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errBodyTooLarge
	}
	return errInvalidJSON
}

// failDecode renders a decodeJSON error (413 for oversized bodies, 400 otherwise).
func failDecode(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errBodyTooLarge) {
		fail(w, r, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "request body too large")
		return
	}
	fail(w, r, http.StatusBadRequest, codeInvalidBody, "invalid json body")
}

func unavailable(w http.ResponseWriter, r *http.Request, msg string) {
	fail(w, r, http.StatusServiceUnavailable, codeUnavailable, msg)
}
