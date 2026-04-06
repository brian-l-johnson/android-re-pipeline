//go:generate swag init -g cmd/server/main.go --output docs

// @title           Android RE Pipeline — Coordinator API
// @version         1.0
// @description     Tracks analysis jobs and serves decompiled APK results.
// @host            coordinator.apps.blj.wtf
// @BasePath        /

package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	httpSwagger "github.com/swaggo/http-swagger"

	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/api"
	_ "github.com/brian-l-johnson/android-re-pipeline/services/coordinator/docs"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/jobs"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/mobsf"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/pipeline"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/queue"
	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/store"
)

//go:embed static
var staticFiles embed.FS

func main() {
	// -----------------------------------------------------------------------
	// Config from environment
	// -----------------------------------------------------------------------
	natsURL       := getEnv("NATS_URL", "nats://localhost:4222")
	databaseURL   := mustEnv("DATABASE_URL")
	jadxImage     := getEnv("JADX_IMAGE", "ghcr.io/brian-l-johnson/re-jadx:latest")
	apktoolImage  := getEnv("APKTOOL_IMAGE", "ghcr.io/brian-l-johnson/re-apktool:latest")
	mobsfURL      := getEnv("MOBSF_URL", "http://localhost:8000")
	mobsfAPIKey   := mustEnv("MOBSF_API_KEY")
	servicePort   := getEnv("SERVICE_PORT", "8080")
	dataDir       := getEnv("DATA_PATH", "/data")
	migrationsDir := getEnv("GOOSE_MIGRATION_DIR", "/migrations")

	// -----------------------------------------------------------------------
	// Run Goose migrations before anything else
	// -----------------------------------------------------------------------
	log.Println("running database migrations...")
	if err := runMigrations(databaseURL, migrationsDir); err != nil {
		log.Fatalf("migrations failed: %v", err)
	}
	log.Println("migrations complete")

	// -----------------------------------------------------------------------
	// Build dependencies
	// -----------------------------------------------------------------------
	s, err := store.New(databaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer s.Close()

	mobsfClient := mobsf.NewClient(mobsfURL, mobsfAPIKey)

	manager, err := jobs.NewManager(jadxImage, apktoolImage)
	if err != nil {
		log.Fatalf("create k8s job manager: %v", err)
	}

	// -----------------------------------------------------------------------
	// Pipeline orchestrator wires store + k8s manager + MobSF together
	// -----------------------------------------------------------------------
	orch := pipeline.NewOrchestrator(s, manager, mobsfClient, dataDir)

	// -----------------------------------------------------------------------
	// Reconcile: recover any jobs that were running when coordinator last died
	// -----------------------------------------------------------------------
	log.Println("reconciling running jobs...")
	bgCtx := context.Background()
	runningJobs, err := s.ListRunningJobs(bgCtx)
	if err != nil {
		log.Printf("warn: list running jobs for reconciliation: %v", err)
	} else if len(runningJobs) > 0 {
		ids := make([]uuid.UUID, len(runningJobs))
		for i, j := range runningJobs {
			ids[i] = j.ID
		}
		if err := manager.ReconcileRunningJobs(bgCtx, ids, orch); err != nil {
			log.Printf("warn: reconcile running jobs: %v", err)
		}
	}
	log.Println("reconciliation complete")

	// -----------------------------------------------------------------------
	// Start background workers
	// -----------------------------------------------------------------------
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()

	// Job watcher
	go func() {
		log.Println("starting k8s job watcher...")
		manager.WatchJobs(workerCtx, orch)
	}()

	// NATS consumer
	consumer, err := queue.NewConsumer(natsURL, orch)
	if err != nil {
		log.Fatalf("create nats consumer: %v", err)
	}
	defer consumer.Close()

	go func() {
		log.Println("starting nats consumer...")
		if err := consumer.Run(workerCtx); err != nil && workerCtx.Err() == nil {
			log.Printf("nats consumer exited: %v", err)
		}
	}()

	// -----------------------------------------------------------------------
	// HTTP server
	// -----------------------------------------------------------------------
	h := api.NewHandler(s, dataDir)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	mux.Handle("/swagger/", httpSwagger.WrapHandler)

	// Serve the web UI from the embedded static directory.
	// Strip the "static/" prefix so index.html is served at /.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("build static fs: %v", err)
	}
	mux.Handle("GET /", http.FileServerFS(staticFS))

	addr := fmt.Sprintf(":%s", servicePort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("coordinator listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	workerCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http server shutdown error: %v", err)
	}

	log.Println("shutdown complete")
}

// runMigrations runs all pending Goose migrations against the given database.
// Uses pgx.ParseConfig so both URL (postgres://...) and key=value DSN formats
// are accepted.
func runMigrations(databaseURL, migrationsDir string) error {
	connConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database URL: %w", err)
	}
	db := stdlib.OpenDB(*connConfig)
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	if err := goose.Up(db, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	return nil
}

// getEnv returns the value of the environment variable named key, or
// defaultVal if the variable is not set.
func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// mustEnv returns the value of the environment variable named key,
// or fatals if it is not set.
func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}
