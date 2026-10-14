// Package upload stores feedback screenshots in Qiniu Kodo via form upload.
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
	"net/textproto"
	"path"
	"regexp"
	"strings"
	"time"

	"gogoga_dictionary/internal/apperr"
)

var ErrUnsupportedImage = errors.New("unsupported image")

const (
	// MaxImageBytes is the largest accepted upload (enforced here and in the Qiniu put policy).
	MaxImageBytes         = 10 << 20
	DefaultKeyPrefix      = "feedback/other"
	uploadTokenTTLSeconds = 300
	allowedMimeLimit      = "image/jpeg;image/png;image/webp;image/heic;image/heif"
)

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

// UploadImage stores content under a key derived from keyPrefix. The content type and extension
// come only from the sniffed magic bytes; the client-declared Content-Type and filename are ignored.
func (s *Service) UploadImage(ctx context.Context, content []byte, keyPrefix string) (Result, error) {
	if len(content) == 0 {
		return Result{}, apperr.Wrap(ErrUnsupportedImage, "empty file")
	}
	if len(content) > MaxImageBytes {
		return Result{}, apperr.Wrap(ErrUnsupportedImage, "file too large")
	}
	img, ok := DetectImage(content)
	if !ok {
		return Result{}, apperr.Wrap(ErrUnsupportedImage, "unsupported image type")
	}

	key := makeObjectKey(NormalizeKeyPrefix(keyPrefix), img.Ext)
	token, err := s.makeUploadToken(key, uploadTokenTTLSeconds)
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
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, path.Base(key)))
	header.Set("Content-Type", img.ContentType)
	part, err := writer.CreatePart(header)
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

// PublicBaseURL is the configured public origin (no trailing slash) for uploaded objects.
func (s *Service) PublicBaseURL() string { return s.publicBaseURL }

func makeObjectKey(prefix, ext string) string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s/%s/%d-%s%s", prefix, now.Format("20060102"), now.Unix(), randomHex(10), ext)
}

var keyPrefixPattern = regexp.MustCompile(`^feedback/[a-z0-9_-]{1,32}$`)

// NormalizeKeyPrefix accepts only "feedback/<issueType>"; anything else becomes DefaultKeyPrefix.
func NormalizeKeyPrefix(raw string) string {
	p := strings.TrimRight(strings.TrimSpace(raw), "/")
	if keyPrefixPattern.MatchString(p) {
		return p
	}
	return DefaultKeyPrefix
}

func (s *Service) makeUploadToken(key string, ttlSeconds int64) (string, error) {
	policy := map[string]any{
		"scope":      fmt.Sprintf("%s:%s", s.bucket, key),
		"deadline":   time.Now().UTC().Unix() + ttlSeconds,
		"fsizeLimit": MaxImageBytes,
		"mimeLimit":  allowedMimeLimit,
		"detectMime": 1,
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
