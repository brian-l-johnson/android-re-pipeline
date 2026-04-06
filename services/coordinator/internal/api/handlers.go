package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/brian-l-johnson/android-re-pipeline/services/coordinator/internal/store"
)

const (
	maxFileServeSize = 100 * 1024 // 100 KB
	defaultMaxSearch = 50
	maxSearchLimit   = 200
	maxTreeEntries   = 1000
	searchTimeout    = 30 * time.Second
)

// StatusResponse is the response body for GET /status/{job_id}.
type StatusResponse struct {
	JobID         string  `json:"job_id"`
	Status        string  `json:"status"`
	PackageName   string  `json:"package_name,omitempty"`
	Version       string  `json:"version,omitempty"`
	Source        string  `json:"source,omitempty"`
	SHA256        string  `json:"sha256,omitempty"`
	SubmittedAt   string  `json:"submitted_at"`
	StartedAt     string  `json:"started_at,omitempty"`
	CompletedAt   string  `json:"completed_at,omitempty"`
	JadxStatus    string  `json:"jadx_status"`
	ApktoolStatus string  `json:"apktool_status"`
	MobSFStatus   string  `json:"mobsf_status"`
	ResultsPath   string  `json:"results_path,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// ToolInfo describes the status and output path of a single analysis tool.
type ToolInfo struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

// MobSFInfo describes the MobSF analysis result.
type MobSFInfo struct {
	Status string      `json:"status"`
	Report interface{} `json:"report,omitempty"`
}

// MetaInfo contains parsed APK manifest metadata.
type MetaInfo struct {
	PackageName string   `json:"package_name,omitempty"`
	Version     string   `json:"version,omitempty"`
	VersionCode int      `json:"version_code,omitempty"`
	MinSDK      int      `json:"min_sdk,omitempty"`
	TargetSDK   int      `json:"target_sdk,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Activities  []string `json:"activities,omitempty"`
	Services    []string `json:"services,omitempty"`
	Receivers   []string `json:"receivers,omitempty"`
}

// ResultsResponse is the response body for GET /results/{job_id}.
type ResultsResponse struct {
	JobID       string    `json:"job_id"`
	Status      string    `json:"status"`
	ResultsPath string    `json:"results_path,omitempty"`
	PackageName string    `json:"package_name,omitempty"`
	Jadx        ToolInfo  `json:"jadx"`
	Apktool     ToolInfo  `json:"apktool"`
	MobSF       MobSFInfo `json:"mobsf"`
	Metadata    *MetaInfo `json:"metadata,omitempty"`
}

// TreeEntry represents a single file or directory in a tree listing.
type TreeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size,omitempty"`
}

// TreeResponse is the response body for GET /results/{job_id}/tree.
type TreeResponse struct {
	Path    string      `json:"path"`
	Entries []TreeEntry `json:"entries"`
}

// SearchMatch is a single search hit within a file.
type SearchMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Context string `json:"context"`
}

// SearchResponse is the response body for GET /results/{job_id}/search.
type SearchResponse struct {
	Query     string        `json:"query"`
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
}

// Handler holds the dependencies needed by the HTTP handlers.
type Handler struct {
	store   *store.Store
	dataDir string
}

// NewHandler creates a Handler with the given store and data directory.
func NewHandler(s *store.Store, dataDir string) *Handler {
	return &Handler{store: s, dataDir: dataDir}
}

// RegisterRoutes registers all coordinator API routes on mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("GET /jobs", h.handleListJobs)
	mux.HandleFunc("GET /status/{job_id}", h.handleStatus)
	mux.HandleFunc("GET /results/{job_id}", h.handleResults)
	mux.HandleFunc("GET /results/{job_id}/download", h.handleDownload)
	mux.HandleFunc("GET /results/{job_id}/tree", h.handleTree)
	mux.HandleFunc("GET /results/{job_id}/file", h.handleFile)
	mux.HandleFunc("GET /results/{job_id}/search", h.handleSearch)
}

// ---------------------------------------------------------------------------
// GET /jobs?limit=N&offset=N
// ---------------------------------------------------------------------------

// JobSummary is a single entry in the jobs list response.
type JobSummary struct {
	JobID         string `json:"job_id"`
	Status        string `json:"status"`
	PackageName   string `json:"package_name,omitempty"`
	Version       string `json:"version,omitempty"`
	Source        string `json:"source,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	SubmittedAt   string `json:"submitted_at"`
	CompletedAt   string `json:"completed_at,omitempty"`
	JadxStatus    string `json:"jadx_status"`
	ApktoolStatus string `json:"apktool_status"`
	MobSFStatus   string `json:"mobsf_status"`
	Error         string `json:"error,omitempty"`
}

// ListJobsResponse is the response body for GET /jobs.
type ListJobsResponse struct {
	Jobs   []JobSummary `json:"jobs"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// handleListJobs handles GET /jobs.
//
// @Summary      List all jobs
// @Description  Returns a paginated list of analysis jobs ordered by submission time descending
// @Tags         jobs
// @Produce      json
// @Param        limit   query     int  false  "Maximum number of results (default 50, max 200)"
// @Param        offset  query     int  false  "Pagination offset (default 0)"
// @Success      200     {object}  api.ListJobsResponse
// @Failure      500     {object}  map[string]string  "error"
// @Router       /jobs [get]
func (h *Handler) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	offset := 0

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	jobs, err := h.store.ListJobs(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	total, err := h.store.CountJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count jobs")
		return
	}

	summaries := make([]JobSummary, 0, len(jobs))
	for _, job := range jobs {
		s := JobSummary{
			JobID:         job.ID.String(),
			Status:        job.Status,
			PackageName:   job.PackageName,
			Version:       job.Version,
			Source:        job.Source,
			SHA256:        job.SHA256,
			SubmittedAt:   job.SubmittedAt.Format(time.RFC3339),
			JadxStatus:    job.JadxStatus,
			ApktoolStatus: job.ApktoolStatus,
			MobSFStatus:   job.MobSFStatus,
		}
		if job.CompletedAt != nil {
			s.CompletedAt = job.CompletedAt.Format(time.RFC3339)
		}
		if job.Error != nil {
			s.Error = *job.Error
		}
		summaries = append(summaries, s)
	}

	writeJSON(w, http.StatusOK, ListJobsResponse{
		Jobs:   summaries,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ---------------------------------------------------------------------------
// GET /health
// ---------------------------------------------------------------------------

// handleHealth handles GET /health.
//
// @Summary      Health check
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]string
// @Router       /health [get]
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// GET /status/{job_id}
// ---------------------------------------------------------------------------

// handleStatus handles GET /status/{job_id}.
//
// @Summary      Get job status
// @Description  Returns the current status of an analysis job
// @Tags         jobs
// @Produce      json
// @Param        job_id  path      string          true  "Job UUID"
// @Success      200     {object}  api.StatusResponse
// @Failure      400     {object}  map[string]string  "invalid job_id"
// @Failure      404     {object}  map[string]string  "job not found"
// @Failure      500     {object}  map[string]string  "error"
// @Router       /status/{job_id} [get]
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	job, err := h.store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	writeJSON(w, http.StatusOK, jobToStatusResponse(job))
}

// ---------------------------------------------------------------------------
// GET /results/{job_id}
// ---------------------------------------------------------------------------

// handleResults handles GET /results/{job_id}.
//
// @Summary      Get job results
// @Description  Returns analysis results for a completed job, including tool outputs and APK metadata
// @Tags         jobs
// @Produce      json
// @Param        job_id  path      string          true  "Job UUID"
// @Success      200     {object}  api.ResultsResponse
// @Failure      400     {object}  map[string]string  "invalid job_id"
// @Failure      404     {object}  map[string]string  "job not found"
// @Failure      409     {object}  map[string]string  "job not complete"
// @Failure      500     {object}  map[string]string  "error"
// @Router       /results/{job_id} [get]
func (h *Handler) handleResults(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	job, err := h.store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	if job.Status != "complete" {
		writeError(w, http.StatusConflict, "job is not complete (status: "+job.Status+")")
		return
	}

	meta, err := h.store.GetAPKMetadata(r.Context(), jobID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// metadata is optional — continue without it
		meta = nil
	}

	resultsPath := ""
	if job.ResultsPath != nil {
		resultsPath = *job.ResultsPath
	}

	type toolInfo struct {
		Status string `json:"status"`
		Path   string `json:"path"`
	}
	type mobsfInfo struct {
		Status string          `json:"status"`
		Report json.RawMessage `json:"report,omitempty"`
	}
	type metaInfo struct {
		PackageName string   `json:"package_name,omitempty"`
		Version     string   `json:"version,omitempty"`
		VersionCode int      `json:"version_code,omitempty"`
		MinSDK      int      `json:"min_sdk,omitempty"`
		TargetSDK   int      `json:"target_sdk,omitempty"`
		Permissions []string `json:"permissions,omitempty"`
		Activities  []string `json:"activities,omitempty"`
		Services    []string `json:"services,omitempty"`
		Receivers   []string `json:"receivers,omitempty"`
	}

	resp := struct {
		JobID       string     `json:"job_id"`
		Status      string     `json:"status"`
		ResultsPath string     `json:"results_path,omitempty"`
		PackageName string     `json:"package_name,omitempty"`
		Jadx        toolInfo   `json:"jadx"`
		Apktool     toolInfo   `json:"apktool"`
		MobSF       mobsfInfo  `json:"mobsf"`
		Metadata    *metaInfo  `json:"metadata,omitempty"`
	}{
		JobID:       job.ID.String(),
		Status:      job.Status,
		ResultsPath: resultsPath,
		PackageName: job.PackageName,
		Jadx: toolInfo{
			Status: job.JadxStatus,
			Path:   filepath.Join(resultsPath, "jadx"),
		},
		Apktool: toolInfo{
			Status: job.ApktoolStatus,
			Path:   filepath.Join(resultsPath, "apktool"),
		},
		MobSF: mobsfInfo{
			Status: job.MobSFStatus,
			Report: job.MobSFReport,
		},
	}

	if meta != nil {
		resp.Metadata = &metaInfo{
			PackageName: meta.PackageName,
			Version:     meta.Version,
			VersionCode: meta.VersionCode,
			MinSDK:      meta.MinSDK,
			TargetSDK:   meta.TargetSDK,
			Permissions: meta.Permissions,
			Activities:  meta.Activities,
			Services:    meta.Services,
			Receivers:   meta.Receivers,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /results/{job_id}/download?format=zip|tar.gz
// ---------------------------------------------------------------------------

// handleDownload streams the full results directory as a zip or tar.gz archive.
//
// @Summary      Download job results as archive
// @Description  Streams the complete output directory for a completed job as a zip (default) or tar.gz archive
// @Tags         jobs
// @Produce      application/zip
// @Param        job_id  path   string  true   "Job UUID"
// @Param        format  query  string  false  "Archive format: zip (default) or tar.gz"
// @Success      200  {file}   binary  "Archive file"
// @Failure      400  {object} map[string]string  "invalid job_id or format"
// @Failure      404  {object} map[string]string  "job not found or no results"
// @Failure      409  {object} map[string]string  "jadx/apktool not complete"
// @Failure      500  {object} map[string]string  "error"
// @Router       /results/{job_id}/download [get]
func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	job, err := h.store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	// Allow download as soon as jadx and apktool are done; MobSF may still be running.
	if job.JadxStatus != "complete" || job.ApktoolStatus != "complete" {
		writeError(w, http.StatusConflict, "jadx and apktool not yet complete (jadx: "+job.JadxStatus+", apktool: "+job.ApktoolStatus+")")
		return
	}

	resultsDir := filepath.Join(h.dataDir, "output", jobID.String())
	if _, err := os.Stat(resultsDir); os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, "results directory not found")
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "zip"
	}
	if format != "zip" && format != "tar.gz" {
		writeError(w, http.StatusBadRequest, "format must be zip or tar.gz")
		return
	}

	// Build a filename from package info when available, fall back to job ID.
	baseName := jobID.String()
	if job.PackageName != "" {
		baseName = job.PackageName
		if job.Version != "" {
			baseName += "-" + job.Version
		}
	}

	// Give large archives up to 10 minutes to stream before the connection
	// is cut, overriding the server-wide WriteTimeout for this request only.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Minute)); err != nil {
		// Not fatal — the server-level timeout still applies.
		log.Printf("download: set write deadline: %v", err)
	}

	switch format {
	case "zip":
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, baseName))
		if err := streamZip(w, resultsDir); err != nil {
			log.Printf("download: stream zip for job %s: %v", jobID, err)
		}
	case "tar.gz":
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar.gz"`, baseName))
		if err := streamTarGz(w, resultsDir); err != nil {
			log.Printf("download: stream tar.gz for job %s: %v", jobID, err)
		}
	}
}

// streamZip writes all files under rootDir into a zip archive streamed to w.
func streamZip(w io.Writer, rootDir string) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	return filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = rel
		hdr.Method = zip.Deflate

		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		_, err = io.Copy(fw, f)
		return err
	})
}

// streamTarGz writes all files under rootDir into a gzipped tar archive streamed to w.
func streamTarGz(w io.Writer, rootDir string) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		hdr := &tar.Header{
			Name:    rel,
			Size:    info.Size(),
			Mode:    int64(info.Mode()),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
}

// ---------------------------------------------------------------------------
// GET /results/{job_id}/tree?path=<relative_path>
// ---------------------------------------------------------------------------

// handleTree handles GET /results/{job_id}/tree.
//
// @Summary      List directory tree
// @Description  Lists files and subdirectories under the job's output directory at the given relative path
// @Tags         jobs
// @Produce      json
// @Param        job_id  path      string  true   "Job UUID"
// @Param        path    query     string  false  "Relative path within the output directory"
// @Success      200     {object}  api.TreeResponse
// @Failure      400     {object}  map[string]string  "invalid job_id or path"
// @Failure      404     {object}  map[string]string  "path not found"
// @Failure      500     {object}  map[string]string  "error"
// @Router       /results/{job_id}/tree [get]
func (h *Handler) handleTree(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	baseDir := filepath.Join(h.dataDir, "output", jobID.String())
	relPath := r.URL.Query().Get("path")

	// Path traversal protection
	targetDir, ok := safePath(baseDir, relPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "path not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read directory")
		return
	}

	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size,omitempty"`
	}

	results := make([]entry, 0, len(entries))
	for i, de := range entries {
		if i >= maxTreeEntries {
			break
		}
		e := entry{Name: de.Name()}
		if de.IsDir() {
			e.Type = "dir"
		} else {
			e.Type = "file"
			if info, err := de.Info(); err == nil {
				e.Size = info.Size()
			}
		}
		results = append(results, e)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":    relPath,
		"entries": results,
	})
}

// ---------------------------------------------------------------------------
// GET /results/{job_id}/file?path=<relative_path>
// ---------------------------------------------------------------------------

// handleFile handles GET /results/{job_id}/file.
//
// @Summary      Retrieve a file from job output
// @Description  Returns the contents of a file within the job's output directory. Files larger than 100 KB are truncated; the X-Truncated: true header is set in that case.
// @Tags         jobs
// @Produce      plain
// @Param        job_id  path      string  true  "Job UUID"
// @Param        path    query     string  true  "Relative file path within the output directory"
// @Success      200     {string}  string  "File contents"
// @Failure      400     {object}  map[string]string  "invalid job_id or path"
// @Failure      404     {object}  map[string]string  "file not found"
// @Failure      500     {object}  map[string]string  "error"
// @Router       /results/{job_id}/file [get]
func (h *Handler) handleFile(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	baseDir := filepath.Join(h.dataDir, "output", jobID.String())
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}

	// Path traversal protection
	filePath, ok := safePath(baseDir, relPath)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to open file")
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stat file")
		return
	}
	if fi.IsDir() {
		writeError(w, http.StatusBadRequest, "path is a directory, not a file")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if fi.Size() > maxFileServeSize {
		w.Header().Set("X-Truncated", "true")
		lr := io.LimitReader(f, maxFileServeSize)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, lr)
	} else {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
	}
}

// ---------------------------------------------------------------------------
// GET /results/{job_id}/search?q=<query>&max=<limit>
// ---------------------------------------------------------------------------

var searchExtensions = map[string]bool{
	".java":  true,
	".smali": true,
	".xml":   true,
	".json":  true,
}

// handleSearch handles GET /results/{job_id}/search.
//
// @Summary      Search within job output files
// @Description  Performs a substring search across .java, .smali, .xml, and .json files in the job's output directory
// @Tags         jobs
// @Produce      json
// @Param        job_id  path      string  true   "Job UUID"
// @Param        q       query     string  true   "Search query string"
// @Param        max     query     int     false  "Maximum number of results (default 50, max 200)"
// @Success      200     {object}  api.SearchResponse
// @Failure      400     {object}  map[string]string  "invalid job_id or missing query"
// @Failure      500     {object}  map[string]string  "error"
// @Router       /results/{job_id}/search [get]
func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	jobID, err := parseJobID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job_id")
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		writeError(w, http.StatusBadRequest, "q query parameter is required")
		return
	}

	maxStr := r.URL.Query().Get("max")
	maxResults := defaultMaxSearch
	if maxStr != "" {
		if v, err := strconv.Atoi(maxStr); err == nil && v > 0 {
			maxResults = v
		}
	}
	if maxResults > maxSearchLimit {
		maxResults = maxSearchLimit
	}

	baseDir := filepath.Join(h.dataDir, "output", jobID.String())
	// Path traversal protection on base dir itself
	cleanBase := filepath.Clean(baseDir)
	if !strings.HasPrefix(cleanBase, filepath.Clean(h.dataDir)) {
		writeError(w, http.StatusBadRequest, "invalid job path")
		return
	}

	type match struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Context string `json:"context"`
	}

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()

	var matches []match
	truncated := false

	err = filepath.Walk(baseDir, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		if !searchExtensions[ext] {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		lines := strings.Split(string(data), "\n")
		for lineNum, line := range lines {
			if strings.Contains(line, query) {
				if len(matches) >= maxResults {
					truncated = true
					return io.EOF // sentinel to stop walking
				}
				// Compute relative path from baseDir
				rel, _ := filepath.Rel(baseDir, path)
				matches = append(matches, match{
					File:    rel,
					Line:    lineNum + 1,
					Context: strings.TrimSpace(line),
				})
			}
		}
		return nil
	})

	// io.EOF is our stop sentinel, not a real error
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}

	if matches == nil {
		matches = []match{} // return [] not null
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"query":     query,
		"matches":   matches,
		"truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// safePath resolves relPath under baseDir and verifies the result stays within
// baseDir, guarding against path traversal attacks. Returns the resolved path
// and true on success; returns "", false if traversal is detected.
func safePath(baseDir, relPath string) (string, bool) {
	base := filepath.Clean(baseDir)
	target := filepath.Clean(filepath.Join(base, relPath))
	if !strings.HasPrefix(target, base+string(filepath.Separator)) && target != base {
		return "", false
	}
	return target, true
}

// parseJobID extracts and validates the {job_id} path segment.
func parseJobID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("job_id")
	return uuid.Parse(raw)
}

// writeJSON encodes v as JSON and sends it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// jobToStatusResponse converts a store.Job to a flat status response map.
func jobToStatusResponse(job *store.Job) map[string]interface{} {
	resp := map[string]interface{}{
		"job_id":         job.ID.String(),
		"status":         job.Status,
		"package_name":   job.PackageName,
		"version":        job.Version,
		"source":         job.Source,
		"sha256":         job.SHA256,
		"submitted_at":   job.SubmittedAt.Format(time.RFC3339),
		"jadx_status":    job.JadxStatus,
		"apktool_status": job.ApktoolStatus,
		"mobsf_status":   job.MobSFStatus,
	}
	if job.StartedAt != nil {
		resp["started_at"] = job.StartedAt.Format(time.RFC3339)
	}
	if job.CompletedAt != nil {
		resp["completed_at"] = job.CompletedAt.Format(time.RFC3339)
	}
	if job.Error != nil {
		resp["error"] = *job.Error
	}
	if job.ResultsPath != nil {
		resp["results_path"] = *job.ResultsPath
	}
	return resp
}
