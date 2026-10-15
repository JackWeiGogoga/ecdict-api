// Package config loads and validates all environment configuration in one place.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gogoga_dictionary/internal/ratelimit"
)

const (
	DefaultArkURL      = "https://ark.cn-beijing.volces.com/api/v3/responses"
	DefaultArkModels   = "doubao-seed-2-1-lite-260915"
	DefaultTTSSpeakers = "en_female_dacey_uranus_bigtts,en_female_stokie_uranus_bigtts,en_female_allison_uranus_bigtts,en_male_tim_uranus_bigtts,zh_female_vv_uranus_bigtts,zh_male_m191_uranus_bigtts"
	// ShutdownTimeout must stay below docker-compose stop_grace_period.
	ShutdownTimeout = 15 * time.Second
)

type Feishu struct {
	AppID, AppSecret, AppToken, TableID string
}

func (f Feishu) Enabled() bool { return f.AppID != "" }

type Qiniu struct {
	AccessKey, SecretKey, Bucket, UploadURL, PublicBaseURL string
}

func (q Qiniu) Enabled() bool { return q.AccessKey != "" }

type LLM struct {
	URL                string
	APIKey             string
	Models             []string
	MaxBodyBytes       int64
	Timeout            time.Duration
	MaxConcurrent      int
	DailyRequestBudget int
}

func (l LLM) Enabled() bool { return l.APIKey != "" }

type TTS struct {
	APIKey        string
	ResourceID    string
	Speakers      []string
	CacheDir      string
	CacheMaxBytes int64
}

func (t TTS) Enabled() bool { return t.APIKey != "" }

// ASR shares the Volcengine speech API key with TTS.
type ASR struct {
	APIKey     string
	ResourceID string
}

func (a ASR) Enabled() bool { return a.APIKey != "" }

// RateLimits are per-client-IP requests per minute; 0 disables a limiter.
type RateLimits struct {
	LLM, TTS, ASR, Analytics, Feedback, Upload, Search int
}

type Config struct {
	HTTPAddr               string
	DBPath                 string
	SchemaPath             string // optional; empty uses the embedded schema
	TrustedProxies         string
	Resolver               *ratelimit.IPResolver
	AnalyticsRetentionDays int
	RateLimits             RateLimits
	Feishu                 Feishu
	Qiniu                  Qiniu
	LLM                    LLM
	TTS                    TTS
	ASR                    ASR
}

// Load reads the environment. It fails fast on malformed numbers, bad CIDRs and partially
// configured optional features (e.g. some QINIU_* set but not all).
func Load() (Config, error) { return load(os.Getenv) }

func load(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int, min int) int {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < min {
			errs = append(errs, fmt.Errorf("%s must be an integer >= %d", key, min))
			return def
		}
		return n
	}
	list := func(key, def string) []string {
		var out []string
		for _, s := range strings.Split(str(key, def), ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	c := Config{
		HTTPAddr:               str("HTTP_ADDR", ":8080"),
		DBPath:                 str("DB_PATH", "./data/dict.db"),
		SchemaPath:             str("SCHEMA_PATH", ""),
		TrustedProxies:         str("TRUSTED_PROXIES", ratelimit.DefaultTrustedProxies),
		AnalyticsRetentionDays: num("ANALYTICS_RETENTION_DAYS", 395, 0),
		RateLimits: RateLimits{
			LLM:       num("LLM_RATE_PER_MINUTE", 60, 0),
			TTS:       num("TTS_RATE_PER_MINUTE", 120, 0),
			ASR:       num("ASR_RATE_PER_MINUTE", 60, 0),
			Analytics: num("ANALYTICS_RATE_PER_MINUTE", 60, 0),
			Feedback:  num("FEEDBACK_RATE_PER_MINUTE", 10, 0),
			Upload:    num("UPLOAD_RATE_PER_MINUTE", 5, 0),
			Search:    num("SEARCH_RATE_PER_MINUTE", 120, 0),
		},
		Feishu: Feishu{
			AppID:     str("FEISHU_APP_ID", ""),
			AppSecret: str("FEISHU_APP_SECRET", ""),
			AppToken:  str("FEISHU_BITABLE_APP_TOKEN", ""),
			TableID:   str("FEISHU_BITABLE_TABLE_ID", ""),
		},
		Qiniu: Qiniu{
			AccessKey:     str("QINIU_ACCESS_KEY", ""),
			SecretKey:     str("QINIU_SECRET_KEY", ""),
			Bucket:        str("QINIU_BUCKET", ""),
			UploadURL:     str("QINIU_UPLOAD_URL", ""),
			PublicBaseURL: strings.TrimRight(str("QINIU_PUBLIC_BASE_URL", ""), "/"),
		},
		LLM: LLM{
			URL:                str("LLM_ARK_URL", DefaultArkURL),
			APIKey:             str("LLM_ARK_API_KEY", ""),
			Models:             list("LLM_ARK_MODELS", DefaultArkModels),
			MaxBodyBytes:       int64(num("LLM_MAX_BODY_MB", 6, 1)) << 20,
			Timeout:            time.Duration(num("LLM_TIMEOUT_SECONDS", 180, 1)) * time.Second,
			MaxConcurrent:      num("LLM_MAX_CONCURRENT", 32, 1),
			DailyRequestBudget: num("LLM_DAILY_REQUEST_BUDGET", 0, 0),
		},
		TTS: TTS{
			APIKey:        str("VOLC_SPEECH_API_KEY", ""),
			ResourceID:    str("VOLC_TTS_RESOURCE_ID", "seed-tts-2.0"),
			Speakers:      list("VOLC_TTS_SPEAKERS", DefaultTTSSpeakers),
			CacheMaxBytes: int64(num("TTS_CACHE_MAX_MB", 2048, 0)) << 20,
		},
	}
	c.ASR = ASR{
		APIKey:     c.TTS.APIKey,
		ResourceID: str("VOLC_ASR_RESOURCE_ID", "volc.bigasr.auc_turbo"),
	}
	c.TTS.CacheDir = str("TTS_CACHE_DIR", filepath.Join(filepath.Dir(c.DBPath), "tts-cache"))

	resolver, err := ratelimit.NewIPResolver(c.TrustedProxies)
	if err != nil {
		errs = append(errs, fmt.Errorf("TRUSTED_PROXIES: %w", err))
	}
	c.Resolver = resolver

	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_ADDR: %w", err))
	}

	errs = append(errs, allOrNothing("FEISHU_*", map[string]string{
		"FEISHU_APP_ID": c.Feishu.AppID, "FEISHU_APP_SECRET": c.Feishu.AppSecret,
		"FEISHU_BITABLE_APP_TOKEN": c.Feishu.AppToken, "FEISHU_BITABLE_TABLE_ID": c.Feishu.TableID,
	}))
	errs = append(errs, allOrNothing("QINIU_*", map[string]string{
		"QINIU_ACCESS_KEY": c.Qiniu.AccessKey, "QINIU_SECRET_KEY": c.Qiniu.SecretKey,
		"QINIU_BUCKET": c.Qiniu.Bucket, "QINIU_UPLOAD_URL": c.Qiniu.UploadURL,
		"QINIU_PUBLIC_BASE_URL": c.Qiniu.PublicBaseURL,
	}))
	if c.Qiniu.PublicBaseURL != "" && !strings.HasPrefix(c.Qiniu.PublicBaseURL, "https://") && !strings.HasPrefix(c.Qiniu.PublicBaseURL, "http://") {
		errs = append(errs, errors.New("QINIU_PUBLIC_BASE_URL must start with http:// or https://"))
	}
	if c.LLM.Enabled() && len(c.LLM.Models) == 0 {
		errs = append(errs, errors.New("LLM_ARK_MODELS is empty"))
	}
	if c.TTS.Enabled() && len(c.TTS.Speakers) == 0 {
		errs = append(errs, errors.New("VOLC_TTS_SPEAKERS is empty"))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

func allOrNothing(group string, vals map[string]string) error {
	var missing []string
	set := 0
	for k, v := range vals {
		if v == "" {
			missing = append(missing, k)
		} else {
			set++
		}
	}
	if set == 0 || len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%s partially configured; missing %s", group, strings.Join(missing, ", "))
}

// Port returns the port part of HTTP_ADDR (used by -healthcheck).
func (c Config) Port() string {
	_, port, _ := net.SplitHostPort(c.HTTPAddr)
	return port
}

// LogSummary logs the effective configuration without any secret values.
func (c Config) LogSummary(logger *slog.Logger) {
	logger.Info("config",
		"http_addr", c.HTTPAddr,
		"db_path", c.DBPath,
		"schema", schemaSource(c.SchemaPath),
		"trusted_proxies", c.TrustedProxies,
		"analytics_retention_days", c.AnalyticsRetentionDays,
		"rate_per_minute", map[string]int{
			"llm": c.RateLimits.LLM, "tts": c.RateLimits.TTS, "asr": c.RateLimits.ASR, "analytics": c.RateLimits.Analytics,
			"feedback": c.RateLimits.Feedback, "upload": c.RateLimits.Upload, "search": c.RateLimits.Search,
		},
		"feedback_enabled", c.Feishu.Enabled(),
		"upload_enabled", c.Qiniu.Enabled(),
		"qiniu_public_base_url", c.Qiniu.PublicBaseURL,
		"llm_enabled", c.LLM.Enabled(),
		"llm_models", strings.Join(c.LLM.Models, ","),
		"llm_max_body_mb", c.LLM.MaxBodyBytes>>20,
		"llm_max_concurrent", c.LLM.MaxConcurrent,
		"llm_daily_request_budget", c.LLM.DailyRequestBudget,
		"tts_enabled", c.TTS.Enabled(),
		"asr_enabled", c.ASR.Enabled(),
		"tts_cache_dir", c.TTS.CacheDir,
		"tts_cache_max_mb", c.TTS.CacheMaxBytes>>20,
	)
}

func schemaSource(p string) string {
	if p == "" {
		return "embedded"
	}
	return p
}
