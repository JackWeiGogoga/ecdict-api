package feedback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	tenantTokenURLFmt = "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal"
	recordURLFmt      = "https://open.feishu.cn/open-apis/bitable/v1/apps/%s/tables/%s/records"
)

type Config struct {
	AppID      string
	AppSecret  string
	AppToken   string
	TableID    string
	HTTPClient *http.Client
}

type Record struct {
	ClientFeedbackID string
	Content          string
	UserID           string
	Device           string
	IOSVersion       string
	AppVersion       string
	Locale           string
	ScreenshotURL    string
	CreatedAt        time.Time
}

type Service struct {
	appID     string
	appSecret string
	appToken  string
	tableID   string
	client    *http.Client

	mu                  sync.Mutex
	tenantToken         string
	tenantTokenExpireAt time.Time
}

func NewService(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.AppID) == "" {
		return nil, fmt.Errorf("missing AppID")
	}
	if strings.TrimSpace(cfg.AppSecret) == "" {
		return nil, fmt.Errorf("missing AppSecret")
	}
	if strings.TrimSpace(cfg.AppToken) == "" {
		return nil, fmt.Errorf("missing AppToken")
	}
	if strings.TrimSpace(cfg.TableID) == "" {
		return nil, fmt.Errorf("missing TableID")
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}

	return &Service{
		appID:     strings.TrimSpace(cfg.AppID),
		appSecret: strings.TrimSpace(cfg.AppSecret),
		appToken:  strings.TrimSpace(cfg.AppToken),
		tableID:   strings.TrimSpace(cfg.TableID),
		client:    client,
	}, nil
}

func (s *Service) Submit(ctx context.Context, record Record) error {
	tenantToken, err := s.tenantAccessToken(ctx)
	if err != nil {
		return err
	}

	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	fields := map[string]any{
		"client_feedback_id": record.ClientFeedbackID,
		"content":            record.Content,
		"user_id":            record.UserID,
		"device":             record.Device,
		"ios_version":        record.IOSVersion,
		"app_version":        record.AppVersion,
		"locale":             record.Locale,
		"created_at":         createdAt.UnixMilli(),
	}
	if u := strings.TrimSpace(record.ScreenshotURL); u != "" {
		fields["screenshot_url"] = u
	}

	err = s.submitWithToken(ctx, tenantToken, fields)
	return err
}

func (s *Service) tenantAccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.tenantToken != "" && time.Now().Before(s.tenantTokenExpireAt) {
		token := s.tenantToken
		s.mu.Unlock()
		return token, nil
	}
	s.mu.Unlock()

	reqBody := map[string]string{
		"app_id":     s.appID,
		"app_secret": s.appSecret,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal token request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tenantTokenURLFmt, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create token request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request tenant access token failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int64  `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token response failed: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 || strings.TrimSpace(result.TenantAccessToken) == "" {
		return "", fmt.Errorf("get tenant access token failed: status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}

	expiresIn := time.Duration(result.Expire) * time.Second
	if expiresIn <= 0 {
		expiresIn = 2 * time.Hour
	}
	expireAt := time.Now().Add(expiresIn - 2*time.Minute)

	s.mu.Lock()
	s.tenantToken = result.TenantAccessToken
	s.tenantTokenExpireAt = expireAt
	s.mu.Unlock()

	return result.TenantAccessToken, nil
}

func (s *Service) submitWithToken(ctx context.Context, token string, fields map[string]any) error {
	payload := map[string]any{"fields": fields}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal bitable record payload failed: %w", err)
	}

	url := fmt.Sprintf(recordURLFmt, s.appToken, s.tableID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create bitable request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitable request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode bitable response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 {
		return fmt.Errorf("bitable create record failed: status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	return nil
}
