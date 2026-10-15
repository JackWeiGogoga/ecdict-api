package asr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gogoga_dictionary/internal/apperr"
)

func fakeWAV() []byte {
	b := make([]byte, 64)
	copy(b, "RIFF")
	copy(b[8:], "WAVE")
	return b
}

func newTestService(t *testing.T, h http.HandlerFunc) *Service {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewService(Config{APIKey: "k", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecognizeSendsHeadersAndReturnsText(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" || r.Header.Get("X-Api-Resource-Id") != DefaultResourceID ||
			r.Header.Get("X-Api-Sequence") != "-1" || r.Header.Get("X-Api-Request-Id") == "" {
			t.Errorf("headers = %v", r.Header)
		}
		var body upstreamRequest
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		if got, _ := base64.StdEncoding.DecodeString(body.Audio.Data); string(got[:4]) != "RIFF" {
			t.Errorf("audio not forwarded")
		}
		if body.Request.ModelName != "bigmodel" {
			t.Errorf("model_name = %q", body.Request.ModelName)
		}
		w.Header().Set("X-Api-Status-Code", statusOK)
		io.WriteString(w, `{"result":{"text":" 苹果的英文是什么 "}}`)
	})
	text, err := s.Recognize(context.Background(), fakeWAV())
	if err != nil || text != "苹果的英文是什么" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestRecognizeSilentAudioIsEmpty(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", statusSilent)
		io.WriteString(w, `{}`)
	})
	text, err := s.Recognize(context.Background(), fakeWAV())
	if err != nil || text != "" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestRecognizeUpstreamError(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Status-Code", "45000151")
		io.WriteString(w, `{}`)
	})
	_, err := s.Recognize(context.Background(), fakeWAV())
	if err == nil {
		t.Fatal("expected error")
	}
	if _, isValidation := apperr.AsValidation(err); isValidation {
		t.Fatal("upstream errors must not be client-visible validation errors")
	}
}

func TestRecognizeRejectsNonWAV(t *testing.T) {
	s := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	})
	_, err := s.Recognize(context.Background(), []byte("not audio"))
	if !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("err = %v", err)
	}
}
