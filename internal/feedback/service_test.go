package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gogoga_dictionary/internal/apperr"
	"gogoga_dictionary/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.ApplySchema(context.Background(), conn, ""); err != nil {
		t.Fatal(err)
	}
	return conn
}

// fakeFeishu serves the token and record endpoints; failRecords makes record creation fail.
func fakeFeishu(t *testing.T, records *int32, failRecords *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == tenantTokenPath:
			io.WriteString(w, `{"code":0,"tenant_access_token":"tok","expire":7200}`)
		case strings.HasSuffix(r.URL.Path, "/records"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("auth = %q", r.Header.Get("Authorization"))
			}
			var body map[string]map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["fields"]["content"] == "" {
				t.Error("missing content")
			}
			if failRecords != nil && failRecords.Load() {
				io.WriteString(w, `{"code":1254000,"msg":"boom"}`)
				return
			}
			atomic.AddInt32(records, 1)
			io.WriteString(w, `{"code":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSubmitDeduplicates(t *testing.T) {
	var records int32
	var failing atomic.Bool
	srv := fakeFeishu(t, &records, &failing)
	svc, err := NewService(Config{AppID: "a", AppSecret: "s", AppToken: "t", TableID: "tb", BaseURL: srv.URL, DB: testDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{ClientFeedbackID: "id-1", Content: "hello"}

	failing.Store(true)
	if _, err := svc.Submit(context.Background(), rec); err == nil {
		t.Fatal("expected failure")
	}
	failing.Store(false)
	dup, err := svc.Submit(context.Background(), rec) // retry after failure must go through
	if err != nil || dup {
		t.Fatalf("dup=%v err=%v", dup, err)
	}
	dup, err = svc.Submit(context.Background(), rec)
	if err != nil || !dup {
		t.Fatalf("second: dup=%v err=%v", dup, err)
	}
	if records != 1 {
		t.Fatalf("records = %d, want 1", records)
	}
}

func TestNormalize(t *testing.T) {
	base := "https://cdn.example.com"
	long := func(n int) string { return strings.Repeat("é", n) }
	cases := []struct {
		name string
		rec  Record
		base string
		ok   bool
	}{
		{"valid", Record{ClientFeedbackID: " id ", Content: " hi ", ScreenshotURL: base + "/feedback/bug/x.png"}, base, true},
		{"missing id", Record{Content: "hi"}, base, false},
		{"missing content", Record{ClientFeedbackID: "id", Content: "   "}, base, false},
		{"content 4000 runes ok", Record{ClientFeedbackID: "id", Content: long(4000)}, base, true},
		{"content too long", Record{ClientFeedbackID: "id", Content: long(4001)}, base, false},
		{"category too long", Record{ClientFeedbackID: "id", Content: "x", Category: long(65)}, base, false},
		{"user_id too long", Record{ClientFeedbackID: "id", Content: "x", UserID: long(129)}, base, false},
		{"locale too long", Record{ClientFeedbackID: "id", Content: "x", Locale: long(33)}, base, false},
		{"id too long", Record{ClientFeedbackID: long(129), Content: "x"}, base, false},
		{"foreign screenshot", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: "https://evil.example/feedback/x.png"}, base, false},
		{"prefix trick", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: base + ".evil.com/feedback/x"}, base, false},
		{"screenshot outside folder", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: base + "/other/x.png"}, base, false},
		{"no base requires https", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: "http://x/y"}, "", false},
		{"no base https ok", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: "https://x/y"}, "", true},
		{"screenshot too long", Record{ClientFeedbackID: "id", Content: "x", ScreenshotURL: "https://x/" + strings.Repeat("a", 600)}, "", false},
	}
	for _, tc := range cases {
		rec := tc.rec
		err := rec.Normalize(tc.base)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
			continue
		}
		if err != nil {
			if _, isV := apperr.AsValidation(err); !isV {
				t.Errorf("%s: not a validation error", tc.name)
			}
		}
	}
	rec := Record{ClientFeedbackID: " id ", Content: " hi "}
	_ = rec.Normalize("")
	if rec.ClientFeedbackID != "id" || rec.Content != "hi" {
		t.Fatalf("not trimmed: %+v", rec)
	}
}
