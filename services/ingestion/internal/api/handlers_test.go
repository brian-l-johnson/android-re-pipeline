package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/queue"
	"github.com/brian-l-johnson/android-re-pipeline/services/ingestion/internal/sources"
)

// mockPublisher records published messages and optionally returns an error.
type mockPublisher struct {
	ingested []*queue.IngestedMessage
	failed   []*queue.FailedMessage
	err      error
}

func (m *mockPublisher) PublishIngested(_ context.Context, msg *queue.IngestedMessage) error {
	if m.err != nil {
		return m.err
	}
	m.ingested = append(m.ingested, msg)
	return nil
}

func (m *mockPublisher) PublishFailed(_ context.Context, msg *queue.FailedMessage) error {
	if m.err != nil {
		return m.err
	}
	m.failed = append(m.failed, msg)
	return nil
}

// mockSource implements sources.APKSource and returns a fixed body.
type mockSource struct {
	name string
	body string
	err  error
}

func (m *mockSource) Name() string { return m.name }

func (m *mockSource) Download(_ context.Context, identifier string) (io.ReadCloser, *sources.APKMetadata, error) {
	if m.err != nil {
		return nil, nil, m.err
	}
	meta := &sources.APKMetadata{
		Source:      m.name,
		DownloadURL: identifier,
	}
	return io.NopCloser(strings.NewReader(m.body)), meta, nil
}

// newTestHandler returns a Handler wired to a temp data dir and the given publisher.
func newTestHandler(t *testing.T, pub Publisher, srcs ...sources.APKSource) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	apksDir := filepath.Join(dir, "apks")
	if err := os.MkdirAll(apksDir, 0o755); err != nil {
		t.Fatalf("creating apks dir: %v", err)
	}
	h := NewHandler(pub, dir, srcs)
	return h, dir
}

// buildMultipartRequest builds a POST /upload multipart request with the given
// file content under the "file" field.
func buildMultipartRequest(t *testing.T, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "app.apk")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// ---- Upload tests ----

func TestUploadHandler_Success(t *testing.T) {
	pub := &mockPublisher{}
	h, dataDir := newTestHandler(t, pub)

	req := buildMultipartRequest(t, "fake apk content")
	rec := httptest.NewRecorder()
	h.UploadHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	jobID, ok := resp["job_id"]
	if !ok || jobID == "" {
		t.Fatalf("missing job_id in response: %v", resp)
	}

	// APK file must exist on disk
	apkPath := filepath.Join(dataDir, "apks", jobID+".apk")
	if _, err := os.Stat(apkPath); err != nil {
		t.Errorf("APK file not found at %s: %v", apkPath, err)
	}

	// Publisher must have received one IngestedMessage
	if len(pub.ingested) != 1 {
		t.Fatalf("expected 1 ingested message, got %d", len(pub.ingested))
	}
	msg := pub.ingested[0]
	if msg.JobID != jobID {
		t.Errorf("IngestedMessage.JobID = %q, want %q", msg.JobID, jobID)
	}
	if msg.SHA256 == "" {
		t.Error("IngestedMessage.SHA256 should not be empty")
	}
	if msg.Source != "upload" {
		t.Errorf("IngestedMessage.Source = %q, want %q", msg.Source, "upload")
	}
}

func TestUploadHandler_NoFileField(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rec := httptest.NewRecorder()
	h.UploadHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestUploadHandler_PublisherError(t *testing.T) {
	pub := &mockPublisher{err: fmt.Errorf("nats down")}
	h, _ := newTestHandler(t, pub)

	req := buildMultipartRequest(t, "fake content")
	rec := httptest.NewRecorder()
	h.UploadHandler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// ---- Download tests ----

func buildDownloadRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestDownloadHandler_Success(t *testing.T) {
	pub := &mockPublisher{}
	src := &mockSource{name: "directurl", body: "apk bytes"}
	h, dataDir := newTestHandler(t, pub, src)

	body := `{"source":"directurl","identifier":"https://example.com/app.apk"}`
	req := buildDownloadRequest(t, body)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body)
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	jobID := resp["job_id"]
	if jobID == "" {
		t.Fatal("missing job_id in response")
	}

	// APK file must exist on disk
	apkPath := filepath.Join(dataDir, "apks", jobID+".apk")
	if _, err := os.Stat(apkPath); err != nil {
		t.Errorf("APK file not found at %s: %v", apkPath, err)
	}

	if len(pub.ingested) != 1 {
		t.Fatalf("expected 1 ingested message, got %d", len(pub.ingested))
	}
	if pub.ingested[0].Source != "directurl" {
		t.Errorf("Source = %q, want %q", pub.ingested[0].Source, "directurl")
	}
}

func TestDownloadHandler_MissingSource(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	req := buildDownloadRequest(t, `{"identifier":"https://example.com/app.apk"}`)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDownloadHandler_MissingIdentifier(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	req := buildDownloadRequest(t, `{"source":"directurl"}`)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDownloadHandler_UnknownSource(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	req := buildDownloadRequest(t, `{"source":"apkpure","identifier":"com.example.app"}`)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDownloadHandler_SourceError(t *testing.T) {
	pub := &mockPublisher{}
	src := &mockSource{name: "directurl", err: fmt.Errorf("connection refused")}
	h, _ := newTestHandler(t, pub, src)

	req := buildDownloadRequest(t, `{"source":"directurl","identifier":"https://example.com/app.apk"}`)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestDownloadHandler_InvalidJSON(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	req := buildDownloadRequest(t, `not json`)
	rec := httptest.NewRecorder()
	h.DownloadHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---- Health tests ----

func TestHealthHandler(t *testing.T) {
	pub := &mockPublisher{}
	h, _ := newTestHandler(t, pub)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}
