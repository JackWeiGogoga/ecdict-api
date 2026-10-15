// Package asr transcribes short voice clips with Volcengine Doubao big-model file recognition.
// Two upstream modes, chosen by resource ID:
//   - standard (default, "volc.seedasr.auc" = recognition 2.0): submit, then poll query (~1s for a sentence)
//   - flash ("*.auc_turbo"): one request, one result
//
// Audio is forwarded and never stored or logged.
package asr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gogoga_dictionary/internal/apperr"
)

var ErrInvalidAudio = errors.New("invalid audio")

const (
	DefaultBaseURL    = "https://openspeech.bytedance.com/api/v3/auc/bigmodel"
	DefaultResourceID = "volc.seedasr.auc"
	// MaxAudioBytes bounds one clip: ~60s of 16 kHz mono 16-bit WAV.
	MaxAudioBytes = 2 << 20

	statusOK         = "20000000"
	statusProcessing = "20000001"
	statusQueued     = "20000002"
	statusSilent     = "20000003"

	defaultPollInterval = 300 * time.Millisecond
)

type Config struct {
	APIKey     string
	ResourceID string
	// BaseURL is the bigmodel API root; /submit, /query and /recognize/flash are appended.
	BaseURL      string
	Timeout      time.Duration
	PollInterval time.Duration
	HTTPClient   *http.Client
}

type Service struct {
	apiKey       string
	resourceID   string
	baseURL      string
	timeout      time.Duration
	pollInterval time.Duration
	client       *http.Client
}

func NewService(cfg Config) (*Service, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("missing api key")
	}
	s := &Service{
		apiKey:       apiKey,
		resourceID:   strings.TrimSpace(cfg.ResourceID),
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		timeout:      cfg.Timeout,
		pollInterval: cfg.PollInterval,
		client:       cfg.HTTPClient,
	}
	if s.resourceID == "" {
		s.resourceID = DefaultResourceID
	}
	if s.baseURL == "" {
		s.baseURL = DefaultBaseURL
	}
	if s.timeout <= 0 {
		s.timeout = 20 * time.Second
	}
	if s.pollInterval <= 0 {
		s.pollInterval = defaultPollInterval
	}
	if s.client == nil {
		s.client = &http.Client{}
	}
	return s, nil
}

// IsWAV reports whether audio starts with a RIFF/WAVE header, the only format the app sends.
func IsWAV(audio []byte) bool {
	return len(audio) > 44 && string(audio[0:4]) == "RIFF" && string(audio[8:12]) == "WAVE"
}

func (s *Service) flash() bool { return strings.HasSuffix(s.resourceID, "_turbo") }

type upstreamRequest struct {
	User struct {
		UID string `json:"uid"`
	} `json:"user"`
	Audio struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"audio"`
	Request struct {
		ModelName  string `json:"model_name"`
		EnableITN  bool   `json:"enable_itn"`
		EnablePunc bool   `json:"enable_punc"`
	} `json:"request"`
}

type upstreamResponse struct {
	Result struct {
		Text string `json:"text"`
	} `json:"result"`
}

// Recognize returns the transcript of a WAV clip; silent audio yields "".
func (s *Service) Recognize(ctx context.Context, audio []byte) (string, error) {
	if !IsWAV(audio) || len(audio) > MaxAudioBytes {
		return "", apperr.Wrap(ErrInvalidAudio, "audio must be a WAV file up to 2 MB")
	}

	var body upstreamRequest
	body.User.UID = "lingshot"
	body.Audio.Data = base64.StdEncoding.EncodeToString(audio)
	body.Audio.Format = "wav"
	body.Request.ModelName = "bigmodel"
	body.Request.EnableITN = true
	body.Request.EnablePunc = true
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	requestID := newRequestID()

	if s.flash() {
		status, data, err := s.post(ctx, "/recognize/flash", requestID, payload)
		if err != nil {
			return "", err
		}
		return finalText(status, data)
	}

	status, _, err := s.post(ctx, "/submit", requestID, payload)
	if err != nil {
		return "", err
	}
	if status.code != statusOK {
		return "", status.err()
	}
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("asr query: %w", ctx.Err())
		case <-time.After(s.pollInterval):
		}
		status, data, err := s.post(ctx, "/query", requestID, []byte("{}"))
		if err != nil {
			return "", err
		}
		if status.code == statusProcessing || status.code == statusQueued {
			continue
		}
		return finalText(status, data)
	}
}

type upstreamStatus struct {
	http    int
	code    string
	message string
	logID   string
}

func (u upstreamStatus) err() error {
	return fmt.Errorf("upstream http %d status %s: %s logid=%s", u.http, u.code, u.message, u.logID)
}

func finalText(status upstreamStatus, data []byte) (string, error) {
	switch status.code {
	case statusSilent:
		return "", nil
	case statusOK:
		var out upstreamResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("decode upstream: %w logid=%s", err, status.logID)
		}
		return strings.TrimSpace(out.Result.Text), nil
	default:
		return "", status.err()
	}
}

func (s *Service) post(ctx context.Context, path, requestID string, payload []byte) (upstreamStatus, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return upstreamStatus{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", s.apiKey)
	req.Header.Set("X-Api-Resource-Id", s.resourceID)
	req.Header.Set("X-Api-Request-Id", requestID)
	req.Header.Set("X-Api-Sequence", "-1")

	resp, err := s.client.Do(req)
	if err != nil {
		return upstreamStatus{}, nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return upstreamStatus{}, nil, fmt.Errorf("read upstream: %w", err)
	}
	status := upstreamStatus{
		http:    resp.StatusCode,
		code:    resp.Header.Get("X-Api-Status-Code"),
		message: resp.Header.Get("X-Api-Message"),
		logID:   resp.Header.Get("X-Tt-Logid"),
	}
	if resp.StatusCode != http.StatusOK && status.code == "" {
		return status, nil, status.err()
	}
	return status, data, nil
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
