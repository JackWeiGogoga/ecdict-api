package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/config"
	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/feedback"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/repo"
	"gogoga_dictionary/internal/upload"
)

type testEnv struct {
	srv          *httptest.Server
	feishuWrites *int32
	llmBodies    chan map[string]any
}

func newEnv(t *testing.T, limits config.RateLimits) *testEnv {
	t.Helper()
	conn, err := db.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.ApplySchema(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	words := repo.NewWordRepository(conn)
	if err := words.UpsertWord(context.Background(), repo.Word{Word: "apple", Translation: "n. 苹果", FRQ: 3}); err != nil {
		t.Fatal(err)
	}

	env := &testEnv{feishuWrites: new(int32), llmBodies: make(chan map[string]any, 10)}
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/records") {
			atomic.AddInt32(env.feishuWrites, 1)
			io.WriteString(w, `{"code":0}`)
			return
		}
		io.WriteString(w, `{"code":0,"tenant_access_token":"tok","expire":7200}`)
	}))
	t.Cleanup(feishu.Close)
	fb, err := feedback.NewService(feedback.Config{AppID: "a", AppSecret: "s", AppToken: "t", TableID: "x", BaseURL: feishu.URL, DB: conn})
	if err != nil {
		t.Fatal(err)
	}

	ark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		env.llmBodies <- body
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"delta\":\"hi\"}\n\n")
			w.(http.Flusher).Flush()
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"output":[]}`)
	}))
	t.Cleanup(ark.Close)
	llm, err := llmproxy.New(llmproxy.Config{URL: ark.URL, APIKey: "k", AllowedModels: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}

	qiniu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		io.WriteString(w, `{"key":"`+r.FormValue("key")+`"}`)
	}))
	t.Cleanup(qiniu.Close)
	up, err := upload.NewQiniuService(upload.Config{AccessKey: "ak", SecretKey: "sk", Bucket: "b", UploadURL: qiniu.URL, PublicBaseURL: "https://cdn.example.com"})
	if err != nil {
		t.Fatal(err)
	}

	env.srv = httptest.NewServer(NewRouter(Deps{
		DB: conn, Words: words, Analytics: analytics.NewService(conn), Feedback: fb, Upload: up, LLM: llm,
		QiniuPublicBaseURL: "https://cdn.example.com", RateLimits: limits,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}))
	t.Cleanup(env.srv.Close)
	return env
}

func defaultLimits() config.RateLimits {
	return config.RateLimits{LLM: 60, TTS: 120, Analytics: 60, Feedback: 10, Upload: 5, Search: 120}
}

type result struct {
	status int
	header http.Header
	env    map[string]any
	raw    string
}

func (e *testEnv) do(t *testing.T, method, path, ct string, body io.Reader, hdr ...string) result {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	return result{resp.StatusCode, resp.Header, env, string(raw)}
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func TestRoutes(t *testing.T) {
	e := newEnv(t, defaultLimits())
	now := time.Now().UnixMilli()
	event := map[string]any{"event_id": "e1", "event_name": "open", "user_id": "u", "session_id": "s", "platform": "ios",
		"app_version": "1", "build": "1", "system_language": "en", "system_locale": "en_US", "app_language": "en", "event_time_ms": now}
	bad := map[string]any{"event_id": "e2", "event_name": "open", "user_id": "u", "session_id": "s", "platform": "ios",
		"app_version": "1", "build": "1", "system_language": "en", "system_locale": "en_US", "app_language": "en", "event_time_ms": 1}

	cases := []struct {
		name, method, path, ct string
		body                   io.Reader
		status                 int
		code                   string
		check                  func(t *testing.T, r result)
	}{
		{"health", "GET", "/v1/health", "", nil, 200, "", nil},
		{"ready", "GET", "/v1/ready", "", nil, 200, "", func(t *testing.T, r result) {
			f := r.env["data"].(map[string]any)["features"].(map[string]any)
			if f["llm"] != true || f["tts"] != false {
				t.Errorf("features = %v", f)
			}
		}},
		{"word", "GET", "/v1/word/apple", "", nil, 200, "", func(t *testing.T, r result) {
			if r.env["data"].(map[string]any)["word"] != "apple" {
				t.Errorf("data = %v", r.env["data"])
			}
		}},
		{"word case-insensitive", "GET", "/v1/word/Apple", "", nil, 200, "", nil},
		{"word missing", "GET", "/v1/word/", "", nil, 400, "invalid_request", nil},
		{"word not found", "GET", "/v1/word/zzz", "", nil, 404, "not_found", nil},
		{"word wrong method", "POST", "/v1/word/apple", "", nil, 405, "method_not_allowed", nil},
		{"unknown route", "GET", "/v1/nope", "", nil, 404, "not_found", nil},
		{"search", "GET", "/v1/search?q=app&page=999", "", nil, 200, "", func(t *testing.T, r result) {
			d := r.env["data"].(map[string]any)
			if d["page"] != float64(50) || d["mode"] != "prefix" {
				t.Errorf("data = %v", d)
			}
		}},
		{"search fts escaping", "GET", "/v1/search?mode=fuzzy&q=a(*", "", nil, 200, "", nil},
		{"suggest", "GET", "/v1/suggest?q=ap", "", nil, 200, "", func(t *testing.T, r result) {
			items := r.env["data"].(map[string]any)["items"].([]any)
			if len(items) != 1 || items[0].(map[string]any)["translation"] != "n. 苹果" {
				t.Errorf("items = %v", items)
			}
		}},
		{"analytics", "POST", "/v1/analytics/events", "application/json", jsonBody(map[string]any{"events": []any{event, bad}}), 200, "", func(t *testing.T, r result) {
			d := r.env["data"].(map[string]any)
			if d["accepted"] != float64(1) || d["rejected"] != float64(1) {
				t.Errorf("data = %v", d)
			}
		}},
		{"analytics empty", "POST", "/v1/analytics/events", "application/json", strings.NewReader(`{"events":[]}`), 400, "invalid_request", nil},
		{"analytics trailing garbage", "POST", "/v1/analytics/events", "application/json", strings.NewReader(`{"events":[]} x`), 400, "invalid_body", nil},
		{"analytics wrong method", "GET", "/v1/analytics/events", "", nil, 405, "method_not_allowed", nil},
		{"analytics too large", "POST", "/v1/analytics/events", "application/json", strings.NewReader(`{"events":[],"pad":"` + strings.Repeat("x", 300<<10) + `"}`), 413, "body_too_large", nil},
		{"feedback", "POST", "/v1/feedback", "application/json", jsonBody(map[string]any{"client_feedback_id": "f1", "content": "hi", "extra_unknown": 1}), 200, "", nil},
		{"feedback bad screenshot", "POST", "/v1/feedback", "application/json", jsonBody(map[string]any{"client_feedback_id": "f2", "content": "hi", "screenshot_url": "https://evil.example/feedback/x"}), 400, "invalid_request", nil},
		{"feedback too large", "POST", "/v1/feedback", "application/json", jsonBody(map[string]any{"client_feedback_id": "f3", "content": strings.Repeat("x", 40<<10)}), 413, "body_too_large", nil},
		{"tts disabled", "POST", "/v1/tts", "application/json", strings.NewReader(`{}`), 503, "service_unavailable", nil},
		{"tts wrong method", "GET", "/v1/tts", "", nil, 405, "method_not_allowed", nil},
		{"llm non-stream", "POST", "/v1/llm/ark", "application/json", strings.NewReader(`{"model":"m1","input":"x"}`), 200, "", nil},
		{"llm model not allowed", "POST", "/v1/llm/ark", "application/json", strings.NewReader(`{"model":"gpt","input":"x"}`), 400, "invalid_request", nil},
		{"llm missing input", "POST", "/v1/llm/ark", "application/json", strings.NewReader(`{"model":"m1"}`), 400, "invalid_request", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do(t, tc.method, tc.path, tc.ct, tc.body)
			if r.status != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", r.status, tc.status, r.raw)
			}
			if r.header.Get("X-Request-Id") == "" {
				t.Error("missing X-Request-Id")
			}
			if tc.path != "/v1/llm/ark" || tc.status != 200 {
				if r.env["request_id"] == nil || r.env["server_time"] == nil {
					t.Errorf("envelope missing fields: %s", r.raw)
				}
				if r.header.Get("X-Content-Type-Options") != "nosniff" {
					t.Error("missing nosniff")
				}
			}
			if code, _ := r.env["code"].(string); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			if tc.status >= 400 && r.env["error"] == nil {
				t.Errorf("missing error message: %s", r.raw)
			}
			if tc.status == 405 && r.header.Get("Allow") == "" {
				t.Error("missing Allow header")
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
	// Drain bodies recorded by the fake Ark.
	for len(e.llmBodies) > 0 {
		<-e.llmBodies
	}
}

func TestFeedbackDedupThroughAPI(t *testing.T) {
	e := newEnv(t, defaultLimits())
	for i := 0; i < 2; i++ {
		r := e.do(t, "POST", "/v1/feedback", "application/json", jsonBody(map[string]any{"client_feedback_id": "same", "content": "hi"}))
		if r.status != 200 || r.env["data"].(map[string]any)["accepted"] != true {
			t.Fatalf("attempt %d: %d %s", i, r.status, r.raw)
		}
	}
	if *e.feishuWrites != 1 {
		t.Fatalf("feishu writes = %d", *e.feishuWrites)
	}
}

func TestLLMStreamingAndSanitizing(t *testing.T) {
	e := newEnv(t, defaultLimits())
	r := e.do(t, "POST", "/v1/llm/ark", "application/json",
		strings.NewReader(`{"model":"m1","input":"x","stream":true,"store":true,"tools":[],"max_output_tokens":100000}`))
	if r.status != 200 || r.header.Get("Content-Type") != "text/event-stream" || r.raw != "data: {\"delta\":\"hi\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("status=%d ct=%q body=%q", r.status, r.header.Get("Content-Type"), r.raw)
	}
	body := <-e.llmBodies
	if body["store"] != false || body["tools"] != nil || body["max_output_tokens"] != float64(4500) {
		t.Fatalf("forwarded = %v", body)
	}
}

func TestRateLimit429(t *testing.T) {
	limits := defaultLimits()
	limits.Feedback = 1
	e := newEnv(t, limits)
	body := func() io.Reader { return strings.NewReader(`{"client_feedback_id":"x","content":"y"}`) }
	if r := e.do(t, "POST", "/v1/feedback", "application/json", body()); r.status != 200 {
		t.Fatalf("first = %d", r.status)
	}
	r := e.do(t, "POST", "/v1/feedback", "application/json", body(), "X-Request-Id", "abc-123")
	if r.status != 429 || r.header.Get("Retry-After") == "" || r.env["code"] != "rate_limited" || r.env["request_id"] != "abc-123" {
		t.Fatalf("second = %d %v %s", r.status, r.header, r.raw)
	}
	// The test client connects from loopback, a default trusted proxy, so the XFF client is a distinct key.
	if r := e.do(t, "POST", "/v1/feedback", "application/json", body(), "X-Forwarded-For", "198.51.100.1"); r.status != 200 {
		t.Fatalf("other client via trusted proxy = %d", r.status)
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	e := newEnv(t, defaultLimits())
	cases := []struct {
		in       string
		keep     bool
		hexLen32 bool
	}{
		{"abc-DEF-123", true, false},
		{strings.Repeat("a", 64), true, false},
		{strings.Repeat("a", 65), false, true},
		{"bad id\"<>", false, true},
		{"", false, true},
	}
	for _, tc := range cases {
		r := e.do(t, "GET", "/v1/health", "", nil, "X-Request-Id", tc.in)
		got := r.header.Get("X-Request-Id")
		if tc.keep && got != tc.in {
			t.Errorf("%q: got %q", tc.in, got)
		}
		if tc.hexLen32 && (len(got) != 32 || got == tc.in) {
			t.Errorf("%q: generated %q", tc.in, got)
		}
		if r.env["request_id"] != got {
			t.Errorf("envelope id %v != header %q", r.env["request_id"], got)
		}
	}
}

func multipartBody(t *testing.T, prefix string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("key_prefix", prefix)
	fw, _ := mw.CreateFormFile("file", "screenshot.jpg")
	_, _ = fw.Write(content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestUploadImage(t *testing.T) {
	e := newEnv(t, defaultLimits())
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

	body, ct := multipartBody(t, "feedback/bug", png)
	r := e.do(t, "POST", "/v1/feedback/upload-image", ct, body)
	if r.status != 200 {
		t.Fatalf("status = %d %s", r.status, r.raw)
	}
	key := r.env["data"].(map[string]any)["key"].(string)
	if !strings.HasPrefix(key, "feedback/bug/") || !strings.HasSuffix(key, ".png") {
		t.Fatalf("key = %q", key)
	}

	body, ct = multipartBody(t, "../../etc", png)
	r = e.do(t, "POST", "/v1/feedback/upload-image", ct, body)
	if key := r.env["data"].(map[string]any)["key"].(string); !strings.HasPrefix(key, "feedback/other/") {
		t.Fatalf("key = %q", key)
	}

	body, ct = multipartBody(t, "feedback/bug", []byte("<html>not an image</html>"))
	if r := e.do(t, "POST", "/v1/feedback/upload-image", ct, body); r.status != 400 || r.env["error"] != "unsupported image type" {
		t.Fatalf("html upload = %d %s", r.status, r.raw)
	}
}

func TestPanicRecovery(t *testing.T) {
	h := requestIDMiddleware(recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestLogPath(t *testing.T) {
	if logPath("/v1/word/secret") != "/v1/word/:word" || logPath("/v1/search") != "/v1/search" {
		t.Fatal("logPath")
	}
}

func TestLLMBusy503(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	ark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		io.WriteString(w, `{}`)
	}))
	defer ark.Close()
	llm, _ := llmproxy.New(llmproxy.Config{URL: ark.URL, APIKey: "k", AllowedModels: []string{"m1"}, MaxConcurrent: 1})
	srv := httptest.NewServer(NewRouter(Deps{LLM: llm, RateLimits: defaultLimits(), Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}))
	defer srv.Close()
	e := &testEnv{srv: srv}

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.do(t, "POST", "/v1/llm/ark", "application/json", strings.NewReader(`{"model":"m1","input":"x"}`))
	}()
	<-entered
	r := e.do(t, "POST", "/v1/llm/ark", "application/json", strings.NewReader(`{"model":"m1","input":"x"}`))
	close(release)
	<-done
	if r.status != 503 || r.header.Get("Retry-After") == "" || r.env["code"] != "busy" {
		t.Fatalf("%d %v %s", r.status, r.header, r.raw)
	}
}
