// Package asr transcribes short voice clips with Volcengine Doubao big-model "flash" file
// recognition (one request, one result). Audio is forwarded and never stored or logged.
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
	DefaultURL        = "https://openspeech.bytedance.com/api/v3/auc/bigmodel/recognize/flash"
	DefaultResourceID = "volc.bigasr.auc_turbo"
	// MaxAudioBytes bounds one clip: ~60s of 16 kHz mono 16-bit WAV.
	MaxAudioBytes = 2 << 20

	statusOK     = "20000000"
	statusSilent = "20000003"
)

type Config struct {
	APIKey     string
	ResourceID string
	URL        string
	Timeout    time.Duration
	HTTPClient *http.Client
}

type Service struct {
	apiKey     string
	resourceID string
	url        string
	timeout    time.Duration
	client     *http.Client
}

func NewService(cfg Config) (*Service, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("missing api key")
	}
	s := &Service{
		apiKey:     apiKey,
		resourceID: cfg.ResourceID,
		url:        cfg.URL,
		timeout:    cfg.Timeout,
		client:     cfg.HTTPClient,
	}
	if s.resourceID == "" {
		s.resourceID = DefaultResourceID
	}
	if s.url == "" {
		s.url = DefaultURL
	}
	if s.timeout <= 0 {
		s.timeout = 20 * time.Second
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

type upstreamRequest struct {
	User struct {
		UID string `json:"uid"`
	} `json:"user"`
	Audio struct {
		Data string `json:"data"`
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
	body.Request.ModelName = "bigmodel"
	body.Request.EnableITN = true
	body.Request.EnablePunc = true
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", s.apiKey)
	req.Header.Set("X-Api-Resource-Id", s.resourceID)
	req.Header.Set("X-Api-Request-Id", newRequestID())
	req.Header.Set("X-Api-Sequence", "-1")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read upstream: %w", err)
	}

	status := resp.Header.Get("X-Api-Status-Code")
	logID := resp.Header.Get("X-Tt-Logid")
	switch {
	case status == statusSilent:
		return "", nil
	case resp.StatusCode != http.StatusOK || status != statusOK:
		return "", fmt.Errorf("upstream http %d status %s: %s logid=%s",
			resp.StatusCode, status, resp.Header.Get("X-Api-Message"), logID)
	}

	var out upstreamResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode upstream: %w logid=%s", err, logID)
	}
	return strings.TrimSpace(out.Result.Text), nil
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
