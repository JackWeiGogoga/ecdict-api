package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestService(t *testing.T, upstreamURL string) *Service {
	t.Helper()
	svc, err := NewService(Config{
		APIKey:   "secret",
		URL:      upstreamURL,
		CacheDir: t.TempDir(),
		Speakers: []string{"voice_a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSynthesizeConcatenatesChunksAndCaches(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("X-Api-Resource-Id") != "seed-tts-2.0" {
			t.Errorf("headers = %v", r.Header)
		}
		var body upstreamRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.ReqParams.Speaker != "voice_a" || body.ReqParams.AudioParams.SpeechRate != -30 {
			t.Errorf("body = %+v", body)
		}
		// Same framing as the real API: concatenated JSON objects, final code 20000000.
		fmt.Fprintf(w, `{"code":0,"message":"","data":%q}`+"\n", base64.StdEncoding.EncodeToString([]byte("AB")))
		fmt.Fprintf(w, `{"code":0,"message":"","data":%q}`, base64.StdEncoding.EncodeToString([]byte("CD")))
		fmt.Fprint(w, `{"code":0,"message":"","data":"","sentence":{"text":"hi"}}`+"\n")
		fmt.Fprint(w, `{"code":20000000,"message":"OK","data":""}`)
	}))
	defer upstream.Close()

	svc := newTestService(t, upstream.URL)
	req := Request{Text: " hi ", Speaker: "voice_a", SpeechRate: -30}

	audio, cached, err := svc.Synthesize(context.Background(), req)
	if err != nil || cached || string(audio) != "ABCD" {
		t.Fatalf("first: audio=%q cached=%v err=%v", audio, cached, err)
	}
	audio, cached, err = svc.Synthesize(context.Background(), req)
	if err != nil || !cached || string(audio) != "ABCD" {
		t.Fatalf("second: audio=%q cached=%v err=%v", audio, cached, err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestSynthesizeDoesNotCacheLongText(t *testing.T) {
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprintf(w, `{"code":0,"data":%q}{"code":20000000,"message":"OK"}`, base64.StdEncoding.EncodeToString([]byte("X")))
	}))
	defer upstream.Close()

	svc := newTestService(t, upstream.URL)
	req := Request{Text: strings.Repeat("a", 101), Speaker: "voice_a"}
	for i := 0; i < 2; i++ {
		if _, cached, err := svc.Synthesize(context.Background(), req); err != nil || cached {
			t.Fatalf("call %d: cached=%v err=%v", i, cached, err)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestSynthesizeSurfacesUpstreamErrorCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":55000000,"message":"resource ID is mismatched","data":null}`)
	}))
	defer upstream.Close()

	_, _, err := newTestService(t, upstream.URL).Synthesize(context.Background(), Request{Text: "hi", Speaker: "voice_a"})
	if err == nil || !strings.Contains(err.Error(), "55000000") {
		t.Fatalf("err = %v", err)
	}
}

func TestSynthesizeValidatesInput(t *testing.T) {
	svc := newTestService(t, "http://127.0.0.1:1")
	if _, _, err := svc.Synthesize(context.Background(), Request{Text: "  ", Speaker: "voice_a"}); !errors.Is(err, ErrInvalidText) {
		t.Errorf("empty text: err = %v", err)
	}
	if _, _, err := svc.Synthesize(context.Background(), Request{Text: strings.Repeat("a", 601), Speaker: "voice_a"}); !errors.Is(err, ErrInvalidText) {
		t.Errorf("long text: err = %v", err)
	}
	if _, _, err := svc.Synthesize(context.Background(), Request{Text: "hi", Speaker: "other"}); !errors.Is(err, ErrSpeakerNotAllowed) {
		t.Errorf("speaker: err = %v", err)
	}
}
