package feedback

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL    = "https://open.feishu.cn"
	tenantTokenPath   = "/open-apis/auth/v3/tenant_access_token/internal"
	recordURLPathFmt  = "/open-apis/bitable/v1/apps/%s/tables/%s/records"
	maxFeishuRespSize = 64 << 10
)

type Config struct {
	AppID     string
	AppSecret string
	AppToken  string
	TableID   string
	// BaseURL overrides the Feishu open platform origin (tests point it at httptest).
	BaseURL string
	// DB, when set, deduplicates submissions by client_feedback_id (table feedback_submissions).
	DB         *sql.DB
	HTTPClient *http.Client
}

type Record struct {
	ClientFeedbackID string
	Content          string
	Category         string
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
	baseURL   string
	db        *sql.DB
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

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	return &Service{
		baseURL:   baseURL,
		db:        cfg.DB,
		appID:     strings.TrimSpace(cfg.AppID),
		appSecret: strings.TrimSpace(cfg.AppSecret),
		appToken:  strings.TrimSpace(cfg.AppToken),
		tableID:   strings.TrimSpace(cfg.TableID),
		client:    client,
	}, nil
}

// Submit forwards the record to the Feishu bitable. When a DB is configured, a client_feedback_id
// that was already submitted successfully is acknowledged without contacting Feishu again
// (duplicate == true).
func (s *Service) Submit(ctx context.Context, record Record) (duplicate bool, err error) {
	if s.db != nil && record.ClientFeedbackID != "" {
		res, dbErr := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO feedback_submissions (client_feedback_id, created_at_ms) VALUES (?, ?)`,
			record.ClientFeedbackID, time.Now().UnixMilli())
		if dbErr != nil {
			return false, fmt.Errorf("record feedback id: %w", dbErr)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return true, nil
		}
		defer func() {
			if err != nil {
				// Let the client retry the same id after a failed submission.
				_, _ = s.db.ExecContext(context.WithoutCancel(ctx),
					`DELETE FROM feedback_submissions WHERE client_feedback_id = ?`, record.ClientFeedbackID)
			}
		}()
	}
	return false, s.submit(ctx, record)
}

func (s *Service) submit(ctx context.Context, record Record) error {
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
		"category":           record.Category,
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

	return s.submitWithToken(ctx, tenantToken, fields)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+tenantTokenPath, bytes.NewReader(body))
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFeishuRespSize)).Decode(&result); err != nil {
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

	url := s.baseURL + fmt.Sprintf(recordURLPathFmt, s.appToken, s.tableID)
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFeishuRespSize)).Decode(&result); err != nil {
		return fmt.Errorf("decode bitable response failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.Code != 0 {
		return fmt.Errorf("bitable create record failed: status=%d code=%d msg=%s", resp.StatusCode, result.Code, result.Msg)
	}
	return nil
}
