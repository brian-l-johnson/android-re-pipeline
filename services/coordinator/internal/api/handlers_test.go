package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// safePath unit tests
// ---------------------------------------------------------------------------

func TestSafePath_Valid(t *testing.T) {
	base := "/data/output/abc123"
	cases := []struct {
		rel  string
		want string
	}{
		{"", base},
		{"jadx", base + "/jadx"},
		{"jadx/sources/com/example", base + "/jadx/sources/com/example"},
		{"apktool/AndroidManifest.xml", base + "/apktool/AndroidManifest.xml"},
	}

	for _, tc := range cases {
		got, ok := safePath(base, tc.rel)
		if !ok {
			t.Errorf("safePath(%q, %q) returned false, want true", base, tc.rel)
			continue
		}
		if got != tc.want {
			t.Errorf("safePath(%q, %q) = %q, want %q", base, tc.rel, got, tc.want)
		}
	}
}

func TestSafePath_Traversal(t *testing.T) {
	base := "/data/output/abc123"
	cases := []string{
		"../other-job",
		"../../etc/passwd",
		"jadx/../../other-job",
		"../abc123x",
	}

	for _, rel := range cases {
		got, ok := safePath(base, rel)
		if ok {
			t.Errorf("safePath(%q, %q) = %q, ok=true; expected traversal to be rejected", base, rel, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Tree handler tests (uses temp dir, no DB)
// ---------------------------------------------------------------------------

// newHandlerWithTempDir creates a Handler with a nil store (not used in tree/file tests)
// and a temp data directory.
func newHandlerWithTempDir(t *testing.T) (*Handler, string) {
	t.Helper()
	dataDir := t.TempDir()
	h := &Handler{store: nil, dataDir: dataDir}
	return h, dataDir
}

// validJobID is a well-formed UUID used across tests.
const validJobID = "12345678-1234-1234-1234-123456789abc"

func TestHandleTree_ListsFiles(t *testing.T) {
	h, dataDir := newHandlerWithTempDir(t)

	// Create output directory structure for the job
	outDir := filepath.Join(dataDir, "output", validJobID, "jadx")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("creating output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "MainActivity.java"), []byte("class Main {}"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(outDir, "utils"), 0o755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/tree?path=jadx", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleTree(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	entries, ok := resp["entries"].([]interface{})
	if !ok {
		t.Fatalf("entries field missing or wrong type: %v", resp)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(entries), entries)
	}
}

func TestHandleTree_PathNotFound(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/tree?path=nonexistent", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleTree(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleTree_PathTraversal(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/tree?path=../other", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleTree(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleTree_InvalidJobID(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/results/not-a-uuid/tree", nil)
	req.SetPathValue("job_id", "not-a-uuid")
	rec := httptest.NewRecorder()
	h.handleTree(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// File handler tests (uses temp dir, no DB)
// ---------------------------------------------------------------------------

func TestHandleFile_ServesContent(t *testing.T) {
	h, dataDir := newHandlerWithTempDir(t)

	outDir := filepath.Join(dataDir, "output", validJobID, "jadx")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("creating output dir: %v", err)
	}
	wantContent := "class MainActivity { /* decompiled */ }"
	if err := os.WriteFile(filepath.Join(outDir, "MainActivity.java"), []byte(wantContent), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file?path=jadx/MainActivity.java", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != wantContent {
		t.Errorf("body = %q, want %q", got, wantContent)
	}
}

func TestHandleFile_NotFound(t *testing.T) {
	h, dataDir := newHandlerWithTempDir(t)

	// Create the job output dir so the base dir exists
	if err := os.MkdirAll(filepath.Join(dataDir, "output", validJobID), 0o755); err != nil {
		t.Fatalf("creating output dir: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file?path=missing.java", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHandleFile_PathTraversal(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file?path=../etc/passwd", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleFile_MissingPathParam(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleFile_DirectoryRejected(t *testing.T) {
	h, dataDir := newHandlerWithTempDir(t)

	dirPath := filepath.Join(dataDir, "output", validJobID, "jadx")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file?path=jadx", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
}

func TestHandleFile_Truncated(t *testing.T) {
	h, dataDir := newHandlerWithTempDir(t)

	outDir := filepath.Join(dataDir, "output", validJobID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("creating output dir: %v", err)
	}

	// Write a file larger than maxFileServeSize (100 KB)
	bigContent := strings.Repeat("a", maxFileServeSize+1024)
	if err := os.WriteFile(filepath.Join(outDir, "big.java"), []byte(bigContent), 0o644); err != nil {
		t.Fatalf("writing big file: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/results/"+validJobID+"/file?path=big.java", nil)
	req.SetPathValue("job_id", validJobID)
	rec := httptest.NewRecorder()
	h.handleFile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Truncated") != "true" {
		t.Error("expected X-Truncated: true header")
	}
	if len(rec.Body.Bytes()) != maxFileServeSize {
		t.Errorf("body length = %d, want %d", len(rec.Body.Bytes()), maxFileServeSize)
	}
}

// ---------------------------------------------------------------------------
// Health handler test
// ---------------------------------------------------------------------------

func TestHandleHealth(t *testing.T) {
	h, _ := newHandlerWithTempDir(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.handleHealth(rec, req)

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
