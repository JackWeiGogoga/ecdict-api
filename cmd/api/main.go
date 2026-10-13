package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/feedback"
	httpapi "gogoga_dictionary/internal/http"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/repo"
	"gogoga_dictionary/internal/upload"
)

func main() {
	addr := getenv("HTTP_ADDR", ":8080")
	dbPath := getenv("DB_PATH", "./data/dict.db")
	schemaPath := getenv("SCHEMA_PATH", "./migrations/schema.sql")

	if err := os.MkdirAll("./data", 0o755); err != nil {
		log.Fatalf("create data dir failed: %v", err)
	}

	conn, err := db.OpenSQLite(dbPath)
	if err != nil {
		log.Fatalf("open db failed: %v", err)
	}
	defer conn.Close()

	if err := db.ApplySchema(context.Background(), conn, schemaPath); err != nil {
		log.Fatalf("apply schema failed: %v", err)
	}

	wordRepo := repo.NewWordRepository(conn)
	analyticsSvc := analytics.NewService(conn)

	var feedbackSvc *feedback.Service
	feedbackAppID := strings.TrimSpace(getenv("FEISHU_APP_ID", ""))
	feedbackAppSecret := strings.TrimSpace(getenv("FEISHU_APP_SECRET", ""))
	feedbackAppToken := strings.TrimSpace(getenv("FEISHU_BITABLE_APP_TOKEN", ""))
	feedbackTableID := strings.TrimSpace(getenv("FEISHU_BITABLE_TABLE_ID", ""))
	if feedbackAppID != "" && feedbackAppSecret != "" && feedbackAppToken != "" && feedbackTableID != "" {
		svc, err := feedback.NewService(feedback.Config{
			AppID:     feedbackAppID,
			AppSecret: feedbackAppSecret,
			AppToken:  feedbackAppToken,
			TableID:   feedbackTableID,
		})
		if err != nil {
			log.Printf("feedback service disabled: %v", err)
		} else {
			feedbackSvc = svc
			log.Printf("feedback service enabled")
		}
	} else {
		log.Printf("feedback service disabled: missing FEISHU_* env")
	}

	var uploadSvc *upload.Service
	qiniuAK := strings.TrimSpace(getenv("QINIU_ACCESS_KEY", ""))
	qiniuSK := strings.TrimSpace(getenv("QINIU_SECRET_KEY", ""))
	qiniuBucket := strings.TrimSpace(getenv("QINIU_BUCKET", ""))
	qiniuUploadURL := strings.TrimSpace(getenv("QINIU_UPLOAD_URL", ""))
	qiniuPublicBaseURL := strings.TrimSpace(getenv("QINIU_PUBLIC_BASE_URL", ""))
	if qiniuAK != "" && qiniuSK != "" && qiniuBucket != "" && qiniuUploadURL != "" && qiniuPublicBaseURL != "" {
		svc, err := upload.NewQiniuService(upload.Config{
			AccessKey:     qiniuAK,
			SecretKey:     qiniuSK,
			Bucket:        qiniuBucket,
			UploadURL:     qiniuUploadURL,
			PublicBaseURL: qiniuPublicBaseURL,
		})
		if err != nil {
			log.Printf("upload service disabled: %v", err)
		} else {
			uploadSvc = svc
			log.Printf("upload service enabled")
		}
	} else {
		log.Printf("upload service disabled: missing QINIU_* env")
	}

	var llmProxy *llmproxy.Proxy
	proxy, err := llmproxy.New(llmproxy.Config{
		Upstreams: map[string]llmproxy.Upstream{
			"openrouter": {
				URL:           getenv("LLM_OPENROUTER_URL", "https://openrouter.ai/api/v1/chat/completions"),
				APIKey:        getenv("LLM_OPENROUTER_API_KEY", ""),
				Headers:       map[string]string{"X-Title": "Lingshot iOS"},
				AllowedModels: strings.Split(getenv("LLM_OPENROUTER_MODELS", "openai/gpt-4o,openai/gpt-4o-mini,openai/gpt-5-mini,openai/gpt-5.2,bytedance-seed/seed-1.6,deepseek/deepseek-v3.2,google/gemini-3-flash-preview"), ","),
			},
			"ark": {
				URL:           getenv("LLM_ARK_URL", "https://ark.cn-beijing.volces.com/api/v3/responses"),
				APIKey:        getenv("LLM_ARK_API_KEY", ""),
				AllowedModels: strings.Split(getenv("LLM_ARK_MODELS", "doubao-seed-2-1-lite-260915"), ","),
			},
		},
		MaxBodyBytes:  int64(getenvInt("LLM_MAX_BODY_MB", 20)) << 20,
		RatePerMinute: getenvInt("LLM_RATE_PER_MINUTE", 60),
	})
	if err != nil {
		log.Printf("llm proxy disabled: %v", err)
	} else {
		llmProxy = proxy
		log.Printf("llm proxy enabled: %v", proxy.Providers())
	}

	h := httpapi.NewHandler(wordRepo, analyticsSvc, feedbackSvc, uploadSvc, llmProxy)

	mux := http.NewServeMux()
	h.Register(mux)

	server := &http.Server{
		Addr:              addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("api started on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("api stopped")
}

func getenv(key string, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func getenvInt(key string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
