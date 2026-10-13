package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/feedback"
	httpapi "gogoga_dictionary/internal/http"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/ratelimit"
	"gogoga_dictionary/internal/repo"
	"gogoga_dictionary/internal/tts"
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
		URL:           getenv("LLM_ARK_URL", "https://ark.cn-beijing.volces.com/api/v3/responses"),
		APIKey:        getenv("LLM_ARK_API_KEY", ""),
		AllowedModels: strings.Split(getenv("LLM_ARK_MODELS", "doubao-seed-2-1-lite-260915"), ","),
		MaxBodyBytes:  int64(getenvInt("LLM_MAX_BODY_MB", 20)) << 20,
		RatePerMinute: getenvInt("LLM_RATE_PER_MINUTE", 60),
	})
	if err != nil {
		log.Printf("llm proxy disabled: %v", err)
	} else {
		llmProxy = proxy
		log.Printf("llm proxy enabled")
	}

	var ttsSvc *tts.Service
	speechKey := strings.TrimSpace(getenv("VOLC_SPEECH_API_KEY", ""))
	if speechKey != "" {
		svc, err := tts.NewService(tts.Config{
			APIKey:     speechKey,
			ResourceID: getenv("VOLC_TTS_RESOURCE_ID", "seed-tts-2.0"),
			CacheDir:   getenv("TTS_CACHE_DIR", filepath.Join(filepath.Dir(dbPath), "tts-cache")),
			Speakers: strings.Split(getenv("VOLC_TTS_SPEAKERS",
				"en_female_dacey_uranus_bigtts,en_female_stokie_uranus_bigtts,en_female_allison_uranus_bigtts,en_male_tim_uranus_bigtts,zh_female_vv_uranus_bigtts,zh_male_m191_uranus_bigtts"), ","),
		})
		if err != nil {
			log.Printf("tts service disabled: %v", err)
		} else {
			ttsSvc = svc
			log.Printf("tts service enabled")
		}
	} else {
		log.Printf("tts service disabled: missing VOLC_SPEECH_API_KEY")
	}

	h := httpapi.NewHandler(wordRepo, analyticsSvc, feedbackSvc, uploadSvc, llmProxy, ttsSvc, ratelimit.New(getenvInt("TTS_RATE_PER_MINUTE", 120)))

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
