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
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidText       = errors.New("invalid text")
	ErrSpeakerNotAllowed = errors.New("speaker not allowed")
)

const (
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
	HTTPClient    *http.Client
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
	client        *http.Client
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
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
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
		client:        cfg.HTTPClient,
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
		svc.client = &http.Client{Timeout: 30 * time.Second}
	}
	return svc, nil
}

// Synthesize returns mp3 bytes, serving repeated (text, speaker, rate) requests from the disk cache.
func (s *Service) Synthesize(ctx context.Context, req Request) (audio []byte, cached bool, err error) {
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || utf8.RuneCountInString(req.Text) > s.maxTextRunes {
		return nil, false, ErrInvalidText
	}
	if !s.speakers[req.Speaker] {
		return nil, false, fmt.Errorf("%w: %s", ErrSpeakerNotAllowed, req.Speaker)
	}
	req.SpeechRate = clamp(req.SpeechRate, -50, 100)

	cacheable := utf8.RuneCountInString(req.Text) <= s.cacheMaxRunes
	path := s.cachePath(req)
	if cacheable {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data, true, nil
		}
	}

	audio, err = s.callUpstream(ctx, req)
	if err != nil {
		return nil, false, err
	}
	if cacheable {
		// Cache write failures only cost a future re-synthesis; don't fail the request.
		_ = writeFileAtomic(path, audio)
	}
	return audio, false, nil
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
	dec := json.NewDecoder(resp.Body)
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

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
