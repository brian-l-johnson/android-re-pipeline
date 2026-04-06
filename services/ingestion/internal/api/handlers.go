package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/queue"
	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/sources"
	"github.com/google/uuid"
)

// Publisher is the subset of queue.Publisher used by the handlers.
// Using an interface here makes the handlers testable with a mock.
type Publisher interface {
	PublishIngested(ctx context.Context, msg *queue.IngestedMessage) error
	PublishFailed(ctx context.Context, msg *queue.FailedMessage) error
}

// Handler holds shared dependencies for the HTTP handlers.
type Handler struct {
	publisher Publisher
	dataPath  string
	sources   map[string]sources.APKSource
}

// NewHandler creates a Handler with the provided publisher, data path, and sources.
func NewHandler(publisher Publisher, dataPath string, srcs []sources.APKSource) *Handler {
	srcMap := make(map[string]sources.APKSource, len(srcs))
	for _, s := range srcs {
		srcMap[s.Name()] = s
	}
	return &Handler{
		publisher: publisher,
		dataPath:  dataPath,
		sources:   srcMap,
	}
}

// writeJSON writes a JSON-encoded value with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// writeError writes a JSON error body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// apksDir returns the directory where APK files are stored.
func (h *Handler) apksDir() string {
	return filepath.Join(h.dataPath, "apks")
}

// saveAPK streams r to disk at <apksDir>/<jobID>.apk, computing SHA256 inline.
// It returns the hex-encoded SHA256 digest.
func (h *Handler) saveAPK(jobID string, r io.Reader) (string, error) {
	dest := filepath.Join(h.apksDir(), jobID+".apk")

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating APK file: %w", err)
	}
	defer f.Close()

	h256 := sha256.New()
	tee := io.TeeReader(r, h256)

	if _, err := io.Copy(f, tee); err != nil {
		os.Remove(dest) //nolint:errcheck
		return "", fmt.Errorf("writing APK: %w", err)
	}

	return hex.EncodeToString(h256.Sum(nil)), nil
}

// HealthHandler handles GET /health.
//
// @Summary      Health check
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]string
// @Router       /health [get]
func (h *Handler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// UploadHandler handles POST /upload (multipart form).
//
// @Summary      Upload an APK file
// @Description  Accepts a multipart APK upload, writes to PVC, publishes to NATS
// @Tags         ingestion
// @Accept       multipart/form-data
// @Produce      json
// @Param        file  formData  file    true  "APK file"
// @Success      201   {object}  map[string]string  "job_id"
// @Failure      400   {object}  map[string]string  "error"
// @Failure      500   {object}  map[string]string  "error"
// @Router       /upload [post]
func (h *Handler) UploadHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parsing multipart form: "+err.Error())
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading 'file' field: "+err.Error())
		return
	}
	defer file.Close()

	jobID := uuid.NewString()

	sha256sum, err := h.saveAPK(jobID, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saving APK: "+err.Error())
		return
	}

	apkPath := filepath.Join(h.apksDir(), jobID+".apk")
	msg := &queue.IngestedMessage{
		JobID:       jobID,
		APKPath:     apkPath,
		PackageName: "",
		Version:     "",
		Source:      "upload",
		SHA256:      sha256sum,
		SubmittedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := h.publisher.PublishIngested(r.Context(), msg); err != nil {
		// APK is on disk — log the NATS failure but still return the job_id
		// so the caller knows the file was received; coordinator reconciliation
		// will pick it up on restart if needed.
		writeError(w, http.StatusInternalServerError, "publishing to NATS: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"job_id": jobID})
}

// DownloadRequest is the JSON body expected by POST /download.
type DownloadRequest struct {
	Source     string `json:"source"`
	Identifier string `json:"identifier"`
}

// DownloadHandler handles POST /download. It is synchronous: it blocks until the
// APK has been fully written to disk before responding.
//
// @Summary      Download and queue an APK
// @Description  Downloads an APK from a URL and queues it for analysis. Synchronous — blocks until download completes.
// @Tags         ingestion
// @Accept       json
// @Produce      json
// @Param        request  body      api.DownloadRequest  true  "Download request"
// @Success      201      {object}  map[string]string    "job_id"
// @Failure      400      {object}  map[string]string    "error"
// @Failure      500      {object}  map[string]string    "error"
// @Router       /download [post]
func (h *Handler) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	// Apply a generous timeout for large APK downloads.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	var req DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request body: "+err.Error())
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "'source' is required")
		return
	}
	if req.Identifier == "" {
		writeError(w, http.StatusBadRequest, "'identifier' is required")
		return
	}

	src, ok := h.sources[req.Source]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q", req.Source))
		return
	}

	body, meta, err := src.Download(ctx, req.Identifier)
	if err != nil {
		writeError(w, http.StatusBadGateway, "downloading APK: "+err.Error())
		return
	}
	defer body.Close()

	jobID := uuid.NewString()

	sha256sum, err := h.saveAPK(jobID, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saving APK: "+err.Error())
		return
	}

	apkPath := filepath.Join(h.apksDir(), jobID+".apk")
	msg := &queue.IngestedMessage{
		JobID:       jobID,
		APKPath:     apkPath,
		PackageName: meta.PackageName,
		Version:     meta.Version,
		Source:      meta.Source,
		SHA256:      sha256sum,
		SubmittedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := h.publisher.PublishIngested(ctx, msg); err != nil {
		writeError(w, http.StatusInternalServerError, "publishing to NATS: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"job_id": jobID})
}
