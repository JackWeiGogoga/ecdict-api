package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"gogoga_dictionary/internal/ratelimit"
)

type ctxKey int

const requestIDKey ctxKey = 0

// requestIDFrom returns the id assigned by requestIDMiddleware (or a fresh one if the handler is
// used without the middleware, e.g. in unit tests).
func requestIDFrom(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok && v != "" {
		return v
	}
	return newRequestID()
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// validClientRequestID accepts at most 64 chars of [A-Za-z0-9-].
func validClientRequestID(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validClientRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// statusRecorder captures status and size for the access log. It implements Flush and Unwrap so SSE
// streaming and http.ResponseController (write deadlines) keep working through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// recoverMiddleware turns handler panics into a logged 500 (when nothing was written yet).
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w}
		}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			slog.Error("panic in handler", "panic", p, "request_id", requestIDFrom(r), "stack", string(debug.Stack()))
			if rec.status == 0 {
				fail(rec, r, http.StatusInternalServerError, codeInternal, "internal error")
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// logPath avoids logging user lookups verbatim.
func logPath(p string) string {
	if strings.HasPrefix(p, "/v1/word/") {
		return "/v1/word/:word"
	}
	return p
}

func accessLogMiddleware(logger *slog.Logger, resolver *ratelimit.IPResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "http",
			slog.String("method", r.Method),
			slog.String("path", logPath(r.URL.Path)),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("request_id", requestIDFrom(r)),
			slog.String("client_ip", resolver.ClientIP(r)),
		)
	})
}

// jsonFallback renders the mux's own 404/405 responses as JSON envelopes (keeping the Allow header).
func jsonFallback(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		cw := &captureWriter{header: make(http.Header)}
		mux.ServeHTTP(cw, r)
		switch cw.status {
		case http.StatusNotFound:
			fail(w, r, http.StatusNotFound, codeNotFound, "not found")
		case http.StatusMethodNotAllowed:
			if allow := cw.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			fail(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
		default: // e.g. redirects for non-canonical paths
			for k, v := range cw.header {
				w.Header()[k] = v
			}
			w.WriteHeader(cw.status)
			_, _ = w.Write(cw.body)
		}
	})
}

type captureWriter struct {
	header http.Header
	status int
	body   []byte
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if len(c.body) < 4096 {
		c.body = append(c.body, b...)
	}
	return len(b), nil
}
