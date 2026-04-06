//go:generate swag init -g cmd/server/main.go --output docs

// @title           Android RE Pipeline — Ingestion API
// @version         1.0
// @description     Accepts APK submissions and queues them for analysis.
// @host            ingestion.apps.blj.wtf
// @BasePath        /

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	httpSwagger "github.com/swaggo/http-swagger"

	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/api"
	_ "github.com/brian-l-johnson/android-re-pipeline/services/ingestion/docs"
	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/queue"
	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/sources"
)

func getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	natsURL := getenv("NATS_URL", "nats://localhost:4222")
	servicePort := getenv("SERVICE_PORT", "8080")
	dataPath := getenv("DATA_PATH", "/data")

	// Ensure the APK storage directory exists.
	apksDir := filepath.Join(dataPath, "apks")
	if err := os.MkdirAll(apksDir, 0o755); err != nil {
		slog.Error("creating APK directory", "path", apksDir, "err", err)
		os.Exit(1)
	}

	pub, err := queue.NewPublisher(natsURL)
	if err != nil {
		slog.Error("connecting to NATS", "url", natsURL, "err", err)
		os.Exit(1)
	}
	defer pub.Close()

	srcs := []sources.APKSource{
		sources.NewDirectURL(),
	}

	h := api.NewHandler(pub, dataPath, srcs)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.UploadHandler)
	mux.HandleFunc("POST /download", h.DownloadHandler)
	mux.HandleFunc("GET /health", h.HealthHandler)
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	srv := &http.Server{
		Addr:         ":" + servicePort,
		Handler:      mux,
		ReadTimeout:  15 * time.Minute, // generous for large multipart uploads
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background.
	go func() {
		slog.Info("ingestion service starting", "port", servicePort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	// Block until SIGTERM or SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	slog.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}
