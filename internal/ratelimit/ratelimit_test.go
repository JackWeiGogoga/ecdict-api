package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	res, err := NewIPResolver("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"untrusted peer ignores xff", "203.0.113.9:1234", []string{"1.2.3.4"}, "203.0.113.9"},
		{"trusted peer no xff", "127.0.0.1:1234", nil, "127.0.0.1"},
		{"trusted peer single hop", "127.0.0.1:1234", []string{"198.51.100.7"}, "198.51.100.7"},
		{"spoofed leftmost ignored", "127.0.0.1:1234", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"skips trusted hops", "172.18.0.2:1234", []string{"6.6.6.6, 198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		{"multiple headers", "127.0.0.1:1", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"all trusted uses leftmost", "127.0.0.1:1", []string{"10.0.0.1, 192.168.1.1"}, "10.0.0.1"},
		{"garbage hop falls back to peer", "127.0.0.1:1", []string{"6.6.6.6, not-an-ip"}, "127.0.0.1"},
		{"ipv6 peer", "[2001:db8::1]:443", []string{"1.2.3.4"}, "2001:db8::1"},
		{"ula proxy", "[fd00::2]:443", []string{"2001:db8::5"}, "2001:db8::5"},
		{"mapped v4", "[::ffff:127.0.0.1]:1", []string{"198.51.100.8"}, "198.51.100.8"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := res.ClientIP(r); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCustomTrustedProxies(t *testing.T) {
	res, err := NewIPResolver("203.0.113.0/24, 10.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1" // loopback no longer trusted
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := res.ClientIP(r); got != "127.0.0.1" {
		t.Fatalf("got %q", got)
	}
	r.RemoteAddr = "10.1.1.1:1"
	if got := res.ClientIP(r); got != "1.1.1.1" {
		t.Fatalf("got %q", got)
	}
	if _, err := NewIPResolver("nonsense/99"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLimiterRetryAfterAndRefill(t *testing.T) {
	l := New(60, nil)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 60; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d denied", i)
		}
	}
	ok, wait := l.Check("a")
	if ok || wait <= 0 || wait > time.Second {
		t.Fatalf("ok=%v wait=%v", ok, wait)
	}
	now = now.Add(time.Second)
	if !l.Allow("a") {
		t.Fatal("expected refill")
	}
	if !l.Allow("b") {
		t.Fatal("other key limited")
	}
}

func TestLimiterKeyCap(t *testing.T) {
	l := New(10, nil)
	l.SetMaxKeys(2)
	if !l.Allow("a") || !l.Allow("b") {
		t.Fatal("first keys denied")
	}
	if l.Allow("c") {
		t.Fatal("new key allowed beyond cap")
	}
	if !l.Allow("a") {
		t.Fatal("existing key denied")
	}
}

func TestMiddleware(t *testing.T) {
	l := New(1, nil)
	h := Middleware(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		rec.Header().Set("X-Request-Id", "rid-1")
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = "203.0.113.1:5"
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := do(); rec.Code != 204 {
		t.Fatalf("first = %d", rec.Code)
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second = %d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["error"] != "too many requests" || env["code"] != "rate_limited" || env["request_id"] != "rid-1" || env["server_time"] == "" {
		t.Fatalf("envelope = %v", env)
	}
	if Middleware(New(0, nil), http.NotFoundHandler()) == nil {
		t.Fatal("disabled limiter should pass through")
	}
}
