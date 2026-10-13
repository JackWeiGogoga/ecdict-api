package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnknownProvider = errors.New("unknown provider")
	ErrModelNotAllowed = errors.New("model not allowed")
	ErrRateLimited     = errors.New("rate limited")
	ErrBodyTooLarge    = errors.New("request body too large")
	ErrInvalidBody     = errors.New("invalid json body")
)

// Upstream is one LLM provider the proxy forwards to. The API key never leaves the server.
type Upstream struct {
	URL           string
	APIKey        string
	Headers       map[string]string
	AllowedModels []string
}

type Config struct {
	Upstreams     map[string]Upstream
	MaxBodyBytes  int64
	RatePerMinute int
	Timeout       time.Duration
	HTTPClient    *http.Client
}

type Proxy struct {
	upstreams    map[string]upstream
	maxBodyBytes int64
	timeout      time.Duration
	client       *http.Client
	limiter      *rateLimiter
}

type upstream struct {
	url     string
	apiKey  string
	headers map[string]string
	models  map[string]bool
}

func New(cfg Config) (*Proxy, error) {
	ups := make(map[string]upstream, len(cfg.Upstreams))
	for name, u := range cfg.Upstreams {
		if strings.TrimSpace(u.URL) == "" || strings.TrimSpace(u.APIKey) == "" {
			continue
		}
		models := make(map[string]bool, len(u.AllowedModels))
		for _, m := range u.AllowedModels {
			if m = strings.TrimSpace(m); m != "" {
				models[m] = true
			}
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("provider %s: empty model allowlist", name)
		}
		ups[name] = upstream{
			url:     strings.TrimSpace(u.URL),
			apiKey:  strings.TrimSpace(u.APIKey),
			headers: u.Headers,
			models:  models,
		}
	}
	if len(ups) == 0 {
		return nil, fmt.Errorf("no provider configured")
	}

	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 20 << 20
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	rate := cfg.RatePerMinute
	if rate <= 0 {
		rate = 60
	}
	client := cfg.HTTPClient
	if client == nil {
		// No client-level timeout: it would cut off long SSE streams. The per-request context bounds it instead.
		client = &http.Client{}
	}

	return &Proxy{
		upstreams:    ups,
		maxBodyBytes: maxBody,
		timeout:      timeout,
		client:       client,
		limiter:      newRateLimiter(rate),
	}, nil
}

func (p *Proxy) Providers() []string {
	out := make([]string, 0, len(p.upstreams))
	for name := range p.upstreams {
		out = append(out, name)
	}
	return out
}

// Forward validates the request and streams the upstream response (JSON or SSE) back unchanged,
// so the client keeps parsing provider-native payloads. Errors returned before anything is
// written to w are safe for the caller to render; once streaming starts it returns nil.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, provider string) error {
	up, ok := p.upstreams[provider]
	if !ok {
		return ErrUnknownProvider
	}
	if !p.limiter.Allow(clientIP(r)) {
		return ErrRateLimited
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBodyBytes))
	if err != nil {
		return ErrBodyTooLarge
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ErrInvalidBody
	}
	model := strings.TrimSpace(probe.Model)
	if !up.models[model] {
		return fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+up.apiKey)
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	for k, v := range up.headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	written, copyErr := copyFlushing(w, resp.Body)
	log.Printf("llm proxy provider=%s model=%s status=%d bytes=%d dur=%s err=%v",
		provider, model, resp.StatusCode, written, time.Since(start).Round(time.Millisecond), copyErr)
	return nil
}

func copyFlushing(w http.ResponseWriter, src io.Reader) (int64, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// clientIP prefers the first X-Forwarded-For hop, which Caddy sets from the real peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a per-key token bucket held in memory; fine for a single API instance.
type rateLimiter struct {
	mu        sync.Mutex
	capacity  float64
	perSecond float64
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{
		capacity:  float64(perMinute),
		perSecond: float64(perMinute) / 60,
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
	}
}

func (l *rateLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.perSecond
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
