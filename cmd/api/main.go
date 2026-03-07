package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/feedback"
	httpapi "gogoga_dictionary/internal/http"
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

	h := httpapi.NewHandler(wordRepo, analyticsSvc, feedbackSvc, uploadSvc)

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

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
