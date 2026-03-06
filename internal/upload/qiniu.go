package upload

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"
)

var ErrInvalidKeyPrefix = errors.New("invalid key prefix")

type Config struct {
	AccessKey     string
	SecretKey     string
	Bucket        string
	UploadURL     string
	PublicBaseURL string
	HTTPClient    *http.Client
}

type Result struct {
	Key string
	URL string
}

type Service struct {
	accessKey     string
	secretKey     string
	bucket        string
	uploadURL     string
	publicBaseURL string
	client        *http.Client
}

func NewQiniuService(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.AccessKey) == "" {
		return nil, fmt.Errorf("missing qiniu access key")
	}
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("missing qiniu secret key")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("missing qiniu bucket")
	}
	if strings.TrimSpace(cfg.UploadURL) == "" {
		return nil, fmt.Errorf("missing qiniu upload url")
	}
	if strings.TrimSpace(cfg.PublicBaseURL) == "" {
		return nil, fmt.Errorf("missing qiniu public base url")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}

	return &Service{
		accessKey:     strings.TrimSpace(cfg.AccessKey),
		secretKey:     strings.TrimSpace(cfg.SecretKey),
		bucket:        strings.TrimSpace(cfg.Bucket),
		uploadURL:     strings.TrimSpace(cfg.UploadURL),
		publicBaseURL: strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/"),
		client:        client,
	}, nil
}

func (s *Service) UploadImage(ctx context.Context, content []byte, contentType string, originalName string, keyPrefix string) (Result, error) {
	if len(content) == 0 {
		return Result{}, fmt.Errorf("empty content")
	}

	key, err := s.makeObjectKey(contentType, originalName, keyPrefix)
	if err != nil {
		return Result{}, err
	}
	token, err := s.makeUploadToken(key, 3600)
	if err != nil {
		return Result{}, err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("token", token); err != nil {
		return Result{}, fmt.Errorf("build multipart token failed: %w", err)
	}
	if err := writer.WriteField("key", key); err != nil {
		return Result{}, fmt.Errorf("build multipart key failed: %w", err)
	}

	part, err := writer.CreateFormFile("file", path.Base(key))
	if err != nil {
		return Result{}, fmt.Errorf("create multipart file failed: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return Result{}, fmt.Errorf("write multipart file failed: %w", err)
	}
	if err := writer.Close(); err != nil {
		return Result{}, fmt.Errorf("close multipart writer failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.uploadURL, body)
	if err != nil {
		return Result{}, fmt.Errorf("create upload request failed: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := s.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("qiniu upload request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("qiniu upload failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		Key string `json:"key"`
	}
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &out)
	}
	if strings.TrimSpace(out.Key) == "" {
		out.Key = key
	}

	return Result{
		Key: out.Key,
		URL: s.publicBaseURL + "/" + out.Key,
	}, nil
}

func (s *Service) makeObjectKey(contentType string, originalName string, keyPrefix string) (string, error) {
	prefix, err := normalizeKeyPrefix(keyPrefix)
	if err != nil {
		return "", err
	}

	day := time.Now().UTC().Format("20060102")
	ext := extensionFromContentType(contentType)
	if ext == "" {
		ext = extensionFromName(originalName)
	}
	if ext == "" {
		ext = ".jpg"
	}
	randomPart := randomHex(10)
	return fmt.Sprintf("%s/%s/%d-%s%s", prefix, day, time.Now().UTC().Unix(), randomPart, ext), nil
}

func normalizeKeyPrefix(raw string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "feedback", nil
	}
	if len(trimmed) > 80 {
		return "", fmt.Errorf("%w: too long", ErrInvalidKeyPrefix)
	}
	if strings.Contains(trimmed, "..") {
		return "", fmt.Errorf("%w: cannot contain '..'", ErrInvalidKeyPrefix)
	}

	parts := strings.Split(trimmed, "/")
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		segment := strings.TrimSpace(p)
		if segment == "" {
			continue
		}
		for _, r := range segment {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return "", fmt.Errorf("%w: only letters, numbers, '-', '_', '.', '/' are allowed", ErrInvalidKeyPrefix)
		}
		cleaned = append(cleaned, segment)
	}
	if len(cleaned) == 0 {
		return "feedback", nil
	}
	return strings.Join(cleaned, "/"), nil
}

func (s *Service) makeUploadToken(key string, ttlSeconds int64) (string, error) {
	policy := map[string]any{
		"scope":    fmt.Sprintf("%s:%s", s.bucket, key),
		"deadline": time.Now().UTC().Unix() + ttlSeconds,
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("marshal put policy failed: %w", err)
	}

	enc := base64.URLEncoding
	encodedPolicy := enc.EncodeToString(policyJSON)
	mac := hmac.New(sha1.New, []byte(s.secretKey))
	_, _ = mac.Write([]byte(encodedPolicy))
	signature := enc.EncodeToString(mac.Sum(nil))
	return s.accessKey + ":" + signature + ":" + encodedPolicy, nil
}

func extensionFromContentType(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	default:
		return ""
	}
}

func extensionFromName(name string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		return ""
	}
	ext := strings.ToLower(path.Ext(n))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif":
		if ext == ".jpeg" {
			return ".jpg"
		}
		return ext
	default:
		return ""
	}
}

func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
