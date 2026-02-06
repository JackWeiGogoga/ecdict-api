package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gogoga_dictionary/internal/db"
	httpapi "gogoga_dictionary/internal/http"
	"gogoga_dictionary/internal/repo"
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
	h := httpapi.NewHandler(wordRepo)

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
