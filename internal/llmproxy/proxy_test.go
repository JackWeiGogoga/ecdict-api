package llmproxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestProxy(t *testing.T, upstreamURL string, rate int) *Proxy {
	t.Helper()
	p, err := New(Config{
		Upstreams: map[string]Upstream{
			"ark": {URL: upstreamURL, APIKey: "secret", AllowedModels: []string{"m1"}},
		},
		RatePerMinute: rate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestForwardInjectsKeyAndStreamsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("body not forwarded: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"delta\":\"a\"}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-should-not-matter")
	rec := httptest.NewRecorder()

	if err := p.Forward(rec, req, "ark"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if want := "data: {\"delta\":\"a\"}\n\ndata: [DONE]\n\n"; rec.Body.String() != want {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestForwardPassesUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad"}`)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL, 10)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1"}`))
	if err := p.Forward(rec, req, "ark"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"bad"}` {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestForwardRejections(t *testing.T) {
	p := newTestProxy(t, "http://127.0.0.1:1", 1)

	cases := []struct {
		name     string
		provider string
		body     string
		want     error
	}{
		{"unknown provider", "openrouter", `{"model":"m1"}`, ErrUnknownProvider},
		{"model not allowed", "ark", `{"model":"other"}`, ErrModelNotAllowed},
		{"rate limited", "ark", `{"model":"other"}`, ErrRateLimited},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/llm/"+tc.provider, strings.NewReader(tc.body))
		err := p.Forward(httptest.NewRecorder(), req, tc.provider)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestNewSkipsProvidersWithoutKey(t *testing.T) {
	_, err := New(Config{Upstreams: map[string]Upstream{"ark": {URL: "http://x", AllowedModels: []string{"m1"}}}})
	if err == nil {
		t.Fatal("expected error when no provider has a key")
	}
}
