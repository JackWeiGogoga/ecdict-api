package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"gogoga_dictionary/internal/ratelimit"
)

var (
	ErrModelNotAllowed = errors.New("model not allowed")
	ErrRateLimited     = errors.New("rate limited")
	ErrBodyTooLarge    = errors.New("request body too large")
	ErrInvalidBody     = errors.New("invalid json body")
)

// Config describes the single upstream (Volcengine Ark) the proxy forwards to.
// The API key never leaves the server.
type Config struct {
	URL           string
	APIKey        string
	AllowedModels []string
	MaxBodyBytes  int64
	RatePerMinute int
	Timeout       time.Duration
	HTTPClient    *http.Client
}

type Proxy struct {
	url          string
	apiKey       string
	models       map[string]bool
	maxBodyBytes int64
	timeout      time.Duration
	client       *http.Client
	limiter      *ratelimit.Limiter
}

func New(cfg Config) (*Proxy, error) {
	url := strings.TrimSpace(cfg.URL)
	apiKey := strings.TrimSpace(cfg.APIKey)
	if url == "" || apiKey == "" {
		return nil, fmt.Errorf("missing upstream url or api key")
	}
	models := make(map[string]bool, len(cfg.AllowedModels))
	for _, m := range cfg.AllowedModels {
		if m = strings.TrimSpace(m); m != "" {
			models[m] = true
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("empty model allowlist")
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
		url:          url,
		apiKey:       apiKey,
		models:       models,
		maxBodyBytes: maxBody,
		timeout:      timeout,
		client:       client,
		limiter:      ratelimit.New(rate),
	}, nil
}

// Forward validates the request and streams the upstream response (JSON or SSE) back unchanged,
// so the client keeps parsing provider-native payloads. Errors returned before anything is
// written to w are safe for the caller to render; once streaming starts it returns nil.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request) error {
	if !p.limiter.Allow(ratelimit.ClientIP(r)) {
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
	if !p.models[model] {
		return fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
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
	log.Printf("llm proxy model=%s status=%d bytes=%d dur=%s err=%v",
		model, resp.StatusCode, written, time.Since(start).Round(time.Millisecond), copyErr)
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
