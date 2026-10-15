package asr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gogoga_dictionary/internal/apperr"
)

func fakeWAV() []byte {
	b := make([]byte, 64)
	copy(b, "RIFF")
	copy(b[8:], "WAVE")
	return b
}

func newTestService(t *testing.T, resourceID string, h http.HandlerFunc) *Service {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewService(Config{APIKey: "k", ResourceID: resourceID, BaseURL: srv.URL, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkHeaders(t *testing.T, r *http.Request, resourceID string) {
	t.Helper()
	if r.Header.Get("X-Api-Key") != "k" || r.Header.Get("X-Api-Resource-Id") != resourceID ||
		r.Header.Get("X-Api-Sequence") != "-1" || r.Header.Get("X-Api-Request-Id") == "" {
		t.Errorf("headers = %v", r.Header)
	}
}

func TestStandardSubmitsThenPollsUntilDone(t *testing.T) {
	var queries atomic.Int32
	var submitID string
	s := newTestService(t, "", func(w http.ResponseWriter, r *http.Request) {
		checkHeaders(t, r, DefaultResourceID)
		switch r.URL.Path {
		case "/submit":
			submitID = r.Header.Get("X-Api-Request-Id")
			var body upstreamRequest
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
			}
			if got, _ := base64.StdEncoding.DecodeString(body.Audio.Data); string(got[:4]) != "RIFF" || body.Audio.Format != "wav" {
				t.Errorf("audio not forwarded")
			}
			w.Header().Set("X-Api-Status-Code", statusOK)
			io.WriteString(w, `{}`)
		case "/query":
			if r.Header.Get("X-Api-Request-Id") != submitID {
				t.Errorf("query must reuse the submit request id")
			}
			if queries.Add(1) < 3 {
				w.Header().Set("X-Api-Status-Code", statusProcessing)
				io.WriteString(w, `{}`)
				return
			}
			w.Header().Set("X-Api-Status-Code", statusOK)
			io.WriteString(w, `{"result":{"text":" 苹果用英文怎么说？ "}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	text, err := s.Recognize(context.Background(), fakeWAV())
	if err != nil || text != "苹果用英文怎么说？" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if queries.Load() != 3 {
		t.Fatalf("queries = %d", queries.Load())
	}
}

func TestFlashIsOneRequest(t *testing.T) {
	s := newTestService(t, "volc.bigasr.auc_turbo", func(w http.ResponseWriter, r *http.Request) {
		checkHeaders(t, r, "volc.bigasr.auc_turbo")
		if r.URL.Path != "/recognize/flash" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("X-Api-Status-Code", statusOK)
		io.WriteString(w, `{"result":{"text":"apple"}}`)
	})
	text, err := s.Recognize(context.Background(), fakeWAV())
	if err != nil || text != "apple" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestSilentAudioIsEmpty(t *testing.T) {
	s := newTestService(t, "volc.bigasr.auc_turbo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", statusSilent)
		io.WriteString(w, `{}`)
	})
	text, err := s.Recognize(context.Background(), fakeWAV())
	if err != nil || text != "" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestUpstreamErrorIsInternal(t *testing.T) {
	s := newTestService(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", "45000030")
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := s.Recognize(context.Background(), fakeWAV())
	if err == nil {
		t.Fatal("expected error")
	}
	if _, isValidation := apperr.AsValidation(err); isValidation {
		t.Fatal("upstream errors must not be client-visible validation errors")
	}
}

func TestPollingStopsAtTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", map[bool]string{true: statusOK, false: statusQueued}[r.URL.Path == "/submit"])
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	s, _ := NewService(Config{APIKey: "k", BaseURL: srv.URL, Timeout: 50 * time.Millisecond, PollInterval: 5 * time.Millisecond})
	if _, err := s.Recognize(context.Background(), fakeWAV()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectsNonWAV(t *testing.T) {
	s := newTestService(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	})
	_, err := s.Recognize(context.Background(), []byte("not audio"))
	if !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("err = %v", err)
	}
}
