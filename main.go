// Command sift is a single-user RSS and Atom reader.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// App holds what the web handlers and the background fetcher share.
type App struct {
	db         *sql.DB
	client     *http.Client
	retention  time.Duration
	interval   time.Duration
	ctx        context.Context // cancelled on shutdown
	refreshing atomic.Bool
	reading    readTracker
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Fatalf("%s must be a positive integer, got %q", key, v)
	}
	return n
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := openDB(env("DB_PATH", "rss.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	app := &App{
		db:        db,
		client:    &http.Client{Timeout: 30 * time.Second},
		retention: time.Duration(envInt("RETENTION_DAYS", 30)) * 24 * time.Hour,
		interval:  time.Duration(envInt("REFRESH_MINUTES", 30)) * time.Minute,
		ctx:       ctx,
	}
	app.reading.timeout = time.Duration(envInt("READ_TIMEOUT_MINUTES", 20)) * time.Minute
	go app.run(ctx)

	addr := env("ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server: %v", err)
	}
}
