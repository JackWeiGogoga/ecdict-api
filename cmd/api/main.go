package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gogoga_dictionary/internal/analytics"
	"gogoga_dictionary/internal/asr"
	"gogoga_dictionary/internal/config"
	"gogoga_dictionary/internal/db"
	"gogoga_dictionary/internal/feedback"
	"gogoga_dictionary/internal/httpapi"
	"gogoga_dictionary/internal/llmproxy"
	"gogoga_dictionary/internal/repo"
	"gogoga_dictionary/internal/tts"
	"gogoga_dictionary/internal/upload"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /v1/health on the local server and exit 0 if healthy (for Docker HEALTHCHECK)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Port()))
	}
	if err := run(cfg, logger); err != nil {
		logger.Error("api failed", "err", err)
		os.Exit(1)
	}
}

func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/v1/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}

func run(cfg config.Config, logger *slog.Logger) error {
	cfg.LogSummary(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	conn, err := db.OpenSQLite(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	if err := db.ApplySchema(ctx, conn, cfg.SchemaPath); err != nil {
		return err
	}

	deps, err := buildDeps(cfg, conn, logger)
	if err != nil {
		return err
	}
	go deps.Analytics.RunRetention(ctx, cfg.AnalyticsRetentionDays)
	if deps.TTS != nil {
		go deps.TTS.RunCacheJanitor(ctx, time.Hour)
	}

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second, // LLM/TTS handlers extend their own deadline
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api started", "addr", cfg.HTTPAddr)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown timed out; closing connections", "err", err)
		_ = server.Close()
	}
	logger.Info("api stopped")
	return nil
}

func buildDeps(cfg config.Config, conn *sql.DB, logger *slog.Logger) (httpapi.Deps, error) {
	deps := httpapi.Deps{
		DB:                 conn,
		Words:              repo.NewWordRepository(conn),
		Analytics:          analytics.NewService(conn),
		QiniuPublicBaseURL: cfg.Qiniu.PublicBaseURL,
		RateLimits:         cfg.RateLimits,
		Resolver:           cfg.Resolver,
		Logger:             logger,
	}

	if cfg.Feishu.Enabled() {
		svc, err := feedback.NewService(feedback.Config{
			AppID:     cfg.Feishu.AppID,
			AppSecret: cfg.Feishu.AppSecret,
			AppToken:  cfg.Feishu.AppToken,
			TableID:   cfg.Feishu.TableID,
			DB:        conn,
		})
		if err != nil {
			return deps, fmt.Errorf("feedback: %w", err)
		}
		deps.Feedback = svc
	}

	if cfg.Qiniu.Enabled() {
		svc, err := upload.NewQiniuService(upload.Config{
			AccessKey:     cfg.Qiniu.AccessKey,
			SecretKey:     cfg.Qiniu.SecretKey,
			Bucket:        cfg.Qiniu.Bucket,
			UploadURL:     cfg.Qiniu.UploadURL,
			PublicBaseURL: cfg.Qiniu.PublicBaseURL,
		})
		if err != nil {
			return deps, fmt.Errorf("upload: %w", err)
		}
		deps.Upload = svc
	}

	if cfg.LLM.Enabled() {
		p, err := llmproxy.New(llmproxy.Config{
			URL:                cfg.LLM.URL,
			APIKey:             cfg.LLM.APIKey,
			AllowedModels:      cfg.LLM.Models,
			MaxBodyBytes:       cfg.LLM.MaxBodyBytes,
			Timeout:            cfg.LLM.Timeout,
			MaxConcurrent:      cfg.LLM.MaxConcurrent,
			DailyRequestBudget: cfg.LLM.DailyRequestBudget,
		})
		if err != nil {
			return deps, fmt.Errorf("llm proxy: %w", err)
		}
		deps.LLM = p
	}

	if cfg.TTS.Enabled() {
		svc, err := tts.NewService(tts.Config{
			APIKey:        cfg.TTS.APIKey,
			ResourceID:    cfg.TTS.ResourceID,
			CacheDir:      cfg.TTS.CacheDir,
			CacheMaxBytes: cfg.TTS.CacheMaxBytes,
			Speakers:      cfg.TTS.Speakers,
		})
		if err != nil {
			return deps, fmt.Errorf("tts: %w", err)
		}
		deps.TTS = svc
	}

	if cfg.ASR.Enabled() {
		svc, err := asr.NewService(asr.Config{APIKey: cfg.ASR.APIKey, ResourceID: cfg.ASR.ResourceID})
		if err != nil {
			return deps, fmt.Errorf("asr: %w", err)
		}
		deps.ASR = svc
	}
	return deps, nil
}
