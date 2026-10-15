// Package llmproxy forwards a sanitized Volcengine Ark Responses API request upstream and streams
// the answer (JSON or SSE) back unchanged. The API key never leaves the server.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrModelNotAllowed = errors.New("model not allowed")
	ErrBodyTooLarge    = errors.New("request body too large")
	ErrInvalidBody     = errors.New("invalid json body")
	ErrMissingInput    = errors.New("missing input")
	ErrBusy            = errors.New("llm proxy busy")
	ErrBudgetExceeded  = errors.New("daily llm request budget exceeded")
)

const (
	DefaultMaxBodyBytes    = 6 << 20
	DefaultMaxOutputTokens = 4500
	DefaultMaxConcurrent   = 32
	// MaxFunctionTools caps the function tool definitions forwarded per request.
	MaxFunctionTools = 16
	DefaultTimeout   = 180 * time.Second
)

// allowedFields is the complete set of top-level keys forwarded upstream. Everything else
// (store, previous_response_id, instructions, ...) is dropped. "tools" is handled separately
// by sanitizeTools: only client-side function tools pass, never paid built-ins like web_search.
var allowedFields = map[string]bool{
	"model":             true,
	"input":             true,
	"stream":            true,
	"thinking":          true,
	"text":              true,
	"temperature":       true,
	"max_output_tokens": true,
}

// Config describes the single upstream (Volcengine Ark) the proxy forwards to.
type Config struct {
	URL           string
	APIKey        string
	AllowedModels []string
	MaxBodyBytes  int64
	Timeout       time.Duration
	// MaxConcurrent bounds in-flight upstream requests across all clients (default 32).
	MaxConcurrent int
	// DailyRequestBudget caps forwarded requests per UTC day; 0 means unlimited.
	DailyRequestBudget int
	HTTPClient         *http.Client
	Now                func() time.Time
}

type Proxy struct {
	url          string
	apiKey       string
	models       map[string]bool
	maxBodyBytes int64
	timeout      time.Duration
	client       *http.Client
	sem          chan struct{}
	now          func() time.Time

	budget    int
	budgetMu  sync.Mutex
	budgetDay string
	budgetN   int
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

	p := &Proxy{
		url:          url,
		apiKey:       apiKey,
		models:       models,
		maxBodyBytes: cfg.MaxBodyBytes,
		timeout:      cfg.Timeout,
		client:       cfg.HTTPClient,
		budget:       cfg.DailyRequestBudget,
		now:          cfg.Now,
	}
	if p.maxBodyBytes <= 0 {
		p.maxBodyBytes = DefaultMaxBodyBytes
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.client == nil {
		// No client-level timeout: it would cut off long SSE streams. The per-request context bounds it instead.
		p.client = &http.Client{}
	}
	if p.now == nil {
		p.now = time.Now
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrent
	}
	p.sem = make(chan struct{}, maxConc)
	return p, nil
}

// Timeout is the upper bound of one proxied request, used by the handler to extend write deadlines.
func (p *Proxy) Timeout() time.Duration { return p.timeout }

// BudgetResetIn is the time until the daily budget resets (next UTC midnight).
func (p *Proxy) BudgetResetIn() time.Duration {
	now := p.now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(now)
}

func (p *Proxy) takeBudget() bool {
	if p.budget <= 0 {
		return true
	}
	day := p.now().UTC().Format("2006-01-02")
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	if day != p.budgetDay {
		p.budgetDay, p.budgetN = day, 0
	}
	if p.budgetN >= p.budget {
		return false
	}
	p.budgetN++
	return true
}

// Sanitize keeps only allowlisted top-level fields, validates model/input, clamps
// max_output_tokens and forces store=false. It returns the re-marshaled body and the model.
func (p *Proxy) Sanitize(body []byte) ([]byte, string, error) {
	var in map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&in); err != nil || in == nil {
		return nil, "", ErrInvalidBody
	}
	if dec.Decode(new(json.RawMessage)) != io.EOF {
		return nil, "", ErrInvalidBody
	}

	var model string
	if raw, ok := in["model"]; !ok || json.Unmarshal(raw, &model) != nil {
		return nil, "", fmt.Errorf("%w: %q", ErrModelNotAllowed, "")
	}
	model = strings.TrimSpace(model)
	if !p.models[model] {
		return nil, "", fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}
	if raw, ok := in["input"]; !ok || isNullOrEmpty(raw) {
		return nil, "", ErrMissingInput
	}

	out := make(map[string]any, len(allowedFields)+1)
	for k, v := range in {
		if allowedFields[k] {
			out[k] = v
		}
	}
	out["model"] = model
	if raw, ok := in["tools"]; ok {
		tools, err := sanitizeTools(raw)
		if err != nil {
			return nil, "", err
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	maxTokens := int64(DefaultMaxOutputTokens)
	if raw, ok := in["max_output_tokens"]; ok && string(raw) != "null" {
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, "", ErrInvalidBody
		}
		if n >= 1 && n < DefaultMaxOutputTokens {
			maxTokens = int64(n)
		}
	}
	out["max_output_tokens"] = maxTokens
	out["store"] = false

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, "", ErrInvalidBody
	}
	return encoded, model, nil
}

// sanitizeTools keeps only {"type":"function"} tools, up to MaxFunctionTools.
func sanitizeTools(raw json.RawMessage) ([]json.RawMessage, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, ErrInvalidBody
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(tool, &head) != nil || head.Type != "function" {
			continue
		}
		if len(kept) == MaxFunctionTools {
			return nil, ErrInvalidBody
		}
		kept = append(kept, tool)
	}
	return kept, nil
}

func isNullOrEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == `""` || s == "[]"
}

// Forward validates the request and streams the upstream response (JSON or SSE) back unchanged,
// so the client keeps parsing provider-native payloads. Errors returned before anything is
// written to w are safe for the caller to render; once streaming starts it returns nil.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		return ErrBusy
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return ErrBodyTooLarge
		}
		return ErrInvalidBody
	}
	body, model, err := p.Sanitize(raw)
	if err != nil {
		return err
	}
	if !p.takeBudget() {
		return ErrBudgetExceeded
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
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	written, copyErr := copyFlushing(w, resp.Body)
	slog.Info("llm proxy", "model", model, "status", resp.StatusCode, "bytes", written,
		"duration_ms", time.Since(start).Milliseconds(), "err", errString(copyErr))
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func copyFlushing(w http.ResponseWriter, src io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			_ = rc.Flush()
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
