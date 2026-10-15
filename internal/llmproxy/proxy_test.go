package llmproxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestProxy(t *testing.T, upstreamURL string, mut ...func(*Config)) *Proxy {
	t.Helper()
	cfg := Config{URL: upstreamURL, APIKey: "secret", AllowedModels: []string{"m1"}}
	for _, m := range mut {
		m(&cfg)
	}
	p, err := New(cfg)
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

	p := newTestProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer client-should-not-matter")
	rec := httptest.NewRecorder()

	if err := p.Forward(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if want := "data: {\"delta\":\"a\"}\n\ndata: [DONE]\n\n"; rec.Body.String() != want {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if !rec.Flushed {
		t.Fatal("expected flushes while streaming")
	}
}

func TestForwardSanitizesBody(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	p := newTestProxy(t, upstream.URL)

	cases := []struct {
		name      string
		body      string
		wantMax   float64
		wantTemp  any
		wantInput bool
	}{
		{"clamped + extras dropped", `{"model":"m1","input":[{"role":"user","content":"x"}],"max_output_tokens":99999,"temperature":0.3,"thinking":{"type":"disabled"},"text":{"format":{"type":"json_object"}},"stream":false,"store":true,"tools":[{"type":"web_search"}],"previous_response_id":"r1","instructions":"evil"}`, 4500, 0.3, true},
		{"default max", `{"model":"m1","input":"hi"}`, 4500, nil, true},
		{"client value kept", `{"model":"m1","input":"hi","max_output_tokens":4200}`, 4200, nil, true},
		{"non-positive max", `{"model":"m1","input":"hi","max_output_tokens":0}`, 4500, nil, true},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(tc.body))
		if err := p.Forward(httptest.NewRecorder(), req); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for k := range got {
			if !allowedFields[k] && k != "store" && k != "tools" {
				t.Errorf("%s: forwarded disallowed field %q", tc.name, k)
			}
		}
		if got["store"] != false {
			t.Errorf("%s: store = %v, want false", tc.name, got["store"])
		}
		if got["max_output_tokens"] != tc.wantMax {
			t.Errorf("%s: max_output_tokens = %v, want %v", tc.name, got["max_output_tokens"], tc.wantMax)
		}
		if got["temperature"] != tc.wantTemp {
			t.Errorf("%s: temperature = %v", tc.name, got["temperature"])
		}
		if _, ok := got["input"]; !ok {
			t.Errorf("%s: input missing", tc.name)
		}
	}
}

func TestSanitizeKeepsOnlyFunctionTools(t *testing.T) {
	p := newTestProxy(t, "http://127.0.0.1:1")
	body := `{"model":"m1","input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"play_song","parameters":{"type":"object"}}]}`
	out, _, err := p.Sanitize([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 1 || got.Tools[0]["name"] != "play_song" {
		t.Fatalf("tools = %v", got.Tools)
	}

	tooMany := make([]string, MaxFunctionTools+1)
	for i := range tooMany {
		tooMany[i] = `{"type":"function","name":"f"}`
	}
	body = `{"model":"m1","input":"hi","tools":[` + strings.Join(tooMany, ",") + `]}`
	if _, _, err := p.Sanitize([]byte(body)); !errors.Is(err, ErrInvalidBody) {
		t.Fatalf("too many tools: err = %v", err)
	}
	if _, _, err := p.Sanitize([]byte(`{"model":"m1","input":"hi","tools":"x"}`)); !errors.Is(err, ErrInvalidBody) {
		t.Fatalf("bad tools: err = %v", err)
	}
}

func TestForwardPassesUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad"}`)
	}))
	defer upstream.Close()

	p := newTestProxy(t, upstream.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","input":"x"}`))
	if err := p.Forward(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"bad"}` {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestForwardRejections(t *testing.T) {
	p := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.MaxBodyBytes = 64 })

	cases := []struct {
		name string
		body string
		want error
	}{
		{"invalid body", `not json`, ErrInvalidBody},
		{"trailing garbage", `{"model":"m1","input":"x"} {}`, ErrInvalidBody},
		{"array body", `[1]`, ErrInvalidBody},
		{"model not allowed", `{"model":"other","input":"x"}`, ErrModelNotAllowed},
		{"missing model", `{"input":"x"}`, ErrModelNotAllowed},
		{"missing input", `{"model":"m1"}`, ErrMissingInput},
		{"null input", `{"model":"m1","input":null}`, ErrMissingInput},
		{"bad max tokens", `{"model":"m1","input":"x","max_output_tokens":"9"}`, ErrInvalidBody},
		{"too large", `{"model":"m1","input":"` + strings.Repeat("x", 100) + `"}`, ErrBodyTooLarge},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(tc.body))
		err := p.Forward(httptest.NewRecorder(), req)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestForwardConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	p := newTestProxy(t, upstream.URL, func(c *Config) { c.MaxConcurrent = 1 })

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","input":"x"}`))
		if err := p.Forward(httptest.NewRecorder(), req); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","input":"x"}`))
	if err := p.Forward(httptest.NewRecorder(), req); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	close(release)
	wg.Wait()
}

func TestDailyBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }))
	defer upstream.Close()
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	p := newTestProxy(t, upstream.URL, func(c *Config) {
		c.DailyRequestBudget = 1
		c.Now = func() time.Time { return now }
	})
	call := func() error {
		req := httptest.NewRequest(http.MethodPost, "/v1/llm/ark", strings.NewReader(`{"model":"m1","input":"x"}`))
		return p.Forward(httptest.NewRecorder(), req)
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if err := call(); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if d := p.BudgetResetIn(); d != time.Hour {
		t.Fatalf("reset in %v", d)
	}
	now = now.Add(2 * time.Hour) // next UTC day
	if err := call(); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Config{URL: "http://x", AllowedModels: []string{"m1"}}); err == nil {
		t.Fatal("expected error without api key")
	}
}
