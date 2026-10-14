// Package tts synthesizes speech with Volcengine Doubao TTS (seed-tts-2.0) and caches short texts' mp3 on disk.
package tts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gogoga_dictionary/internal/apperr"
)

var (
	ErrInvalidText       = errors.New("invalid text")
	ErrSpeakerNotAllowed = errors.New("speaker not allowed")
)

const (
	maxUpstreamBytes  = 32 << 20
	upstreamCodeChunk = 0
	upstreamCodeDone  = 20000000
)

type Config struct {
	APIKey       string
	ResourceID   string
	URL          string
	CacheDir     string
	Speakers     []string
	MaxTextRunes int
	// Only texts up to this length (words, example sentences) are cached. Longer texts are
	// one-off conversation replies: no reuse, and the privacy notice promises not to store them.
	CacheMaxRunes int
	// CacheMaxBytes caps the disk cache; SweepCache deletes the oldest files beyond it (0 = no cap).
	CacheMaxBytes int64
	// UpstreamTimeout bounds one upstream call (default 30s). The call is detached from the client
	// request so a disconnect does not waste an already-paid synthesis.
	UpstreamTimeout time.Duration
	HTTPClient      *http.Client
}

type Request struct {
	Text       string
	Speaker    string
	SpeechRate int // Volcengine speech_rate, [-50, 100], 0 = normal
}

type Service struct {
	apiKey        string
	resourceID    string
	url           string
	cacheDir      string
	speakers      map[string]bool
	maxTextRunes  int
	cacheMaxRunes int
	cacheMaxBytes int64
	timeout       time.Duration
	client        *http.Client

	flightMu sync.Mutex
	flights  map[string]*flight
}

type flight struct {
	done  chan struct{}
	audio []byte
	err   error
}

func NewService(cfg Config) (*Service, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("missing api key")
	}
	speakers := make(map[string]bool, len(cfg.Speakers))
	for _, s := range cfg.Speakers {
		if s = strings.TrimSpace(s); s != "" {
			speakers[s] = true
		}
	}
	if len(speakers) == 0 {
		return nil, fmt.Errorf("empty speaker allowlist")
	}
	if err := os.MkdirAll(cfg.CacheDir, cacheDirPerm); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	svc := &Service{
		apiKey:        apiKey,
		resourceID:    cfg.ResourceID,
		url:           cfg.URL,
		cacheDir:      cfg.CacheDir,
		speakers:      speakers,
		maxTextRunes:  cfg.MaxTextRunes,
		cacheMaxRunes: cfg.CacheMaxRunes,
		cacheMaxBytes: cfg.CacheMaxBytes,
		timeout:       cfg.UpstreamTimeout,
		client:        cfg.HTTPClient,
		flights:       make(map[string]*flight),
	}
	if svc.timeout <= 0 {
		svc.timeout = 30 * time.Second
	}
	if svc.resourceID == "" {
		svc.resourceID = "seed-tts-2.0"
	}
	if svc.url == "" {
		svc.url = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"
	}
	if svc.maxTextRunes <= 0 {
		svc.maxTextRunes = 600
	}
	if svc.cacheMaxRunes <= 0 {
		svc.cacheMaxRunes = 100
	}
	if svc.client == nil {
		svc.client = &http.Client{}
	}
	return svc, nil
}

// Synthesize returns mp3 bytes, serving repeated (text, speaker, rate) requests from the disk cache.
func (s *Service) Synthesize(ctx context.Context, req Request) (audio []byte, cached bool, err error) {
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || utf8.RuneCountInString(req.Text) > s.maxTextRunes {
		return nil, false, apperr.Wrap(ErrInvalidText, "invalid text")
	}
	if !s.speakers[req.Speaker] {
		return nil, false, apperr.Wrap(ErrSpeakerNotAllowed, "speaker not allowed")
	}
	req.SpeechRate = clamp(req.SpeechRate, -50, 100)

	if utf8.RuneCountInString(req.Text) > s.cacheMaxRunes {
		audio, err = s.callDetached(ctx, req)
		return audio, false, err
	}

	path := s.cachePath(req)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		now := time.Now()
		_ = os.Chtimes(path, now, now) // keep hot entries young for the LRU-ish sweep
		return data, true, nil
	}

	// Collapse identical concurrent misses into one upstream call.
	s.flightMu.Lock()
	if f, ok := s.flights[path]; ok {
		s.flightMu.Unlock()
		select {
		case <-f.done:
			return f.audio, false, f.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[path] = f
	s.flightMu.Unlock()

	f.audio, f.err = s.callDetached(ctx, req)
	if f.err == nil {
		// Cache write failures only cost a future re-synthesis; don't fail the request.
		_ = writeFileAtomic(path, f.audio)
	}
	s.flightMu.Lock()
	delete(s.flights, path)
	s.flightMu.Unlock()
	close(f.done)
	return f.audio, false, f.err
}

func (s *Service) callDetached(ctx context.Context, req Request) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	return s.callUpstream(ctx, req)
}

func (s *Service) cachePath(req Request) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", s.resourceID, req.Speaker, req.SpeechRate, req.Text)))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.cacheDir, name[:2], name+".mp3")
}

type upstreamRequest struct {
	ReqParams struct {
		Text        string `json:"text"`
		Speaker     string `json:"speaker"`
		AudioParams struct {
			Format     string `json:"format"`
			SampleRate int    `json:"sample_rate"`
			SpeechRate int    `json:"speech_rate"`
		} `json:"audio_params"`
	} `json:"req_params"`
}

type upstreamChunk struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// callUpstream reads the HTTP chunked response: a stream of JSON objects, each carrying a base64 mp3
// fragment (code 0), terminated by code 20000000. Errors arrive as HTTP 200 with another code.
func (s *Service) callUpstream(ctx context.Context, req Request) ([]byte, error) {
	var body upstreamRequest
	body.ReqParams.Text = req.Text
	body.ReqParams.Speaker = req.Speaker
	body.ReqParams.AudioParams.Format = "mp3"
	body.ReqParams.AudioParams.SampleRate = 24000
	body.ReqParams.AudioParams.SpeechRate = req.SpeechRate
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", s.apiKey)
	httpReq.Header.Set("X-Api-Resource-Id", s.resourceID)
	httpReq.Header.Set("X-Api-Request-Id", newRequestID())

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("upstream http %d: %s logid=%s", resp.StatusCode, snippet, resp.Header.Get("X-Tt-Logid"))
	}

	var audio []byte
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBytes))
	for {
		var chunk upstreamChunk
		if err := dec.Decode(&chunk); err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("upstream stream ended without completion")
			}
			return nil, fmt.Errorf("decode upstream chunk: %w", err)
		}
		switch chunk.Code {
		case upstreamCodeChunk:
			if chunk.Data == "" {
				continue
			}
			part, err := base64.StdEncoding.DecodeString(chunk.Data)
			if err != nil {
				return nil, fmt.Errorf("decode audio chunk: %w", err)
			}
			audio = append(audio, part...)
		case upstreamCodeDone:
			if len(audio) == 0 {
				return nil, fmt.Errorf("upstream returned no audio")
			}
			return audio, nil
		default:
			return nil, fmt.Errorf("upstream code %d: %s logid=%s", chunk.Code, chunk.Message, resp.Header.Get("X-Tt-Logid"))
		}
	}
}

const (
	cacheDirPerm  = 0o750
	cacheFilePerm = 0o640
)

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), cacheDirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), cacheFilePerm); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
