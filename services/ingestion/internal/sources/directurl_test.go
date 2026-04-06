package sources

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectURLDownload_Success(t *testing.T) {
	wantBody := "fake apk bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, wantBody) //nolint:errcheck
	}))
	defer srv.Close()

	d := NewDirectURL()
	rc, meta, err := d.Download(context.Background(), srv.URL+"/app.apk")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("body mismatch: got %q, want %q", got, wantBody)
	}

	if meta == nil {
		t.Fatal("expected non-nil metadata")
	}
	if meta.Source != "directurl" {
		t.Errorf("source = %q, want %q", meta.Source, "directurl")
	}
	if !strings.HasPrefix(meta.DownloadURL, srv.URL) {
		t.Errorf("DownloadURL = %q, want prefix %q", meta.DownloadURL, srv.URL)
	}
	// SHA256 is left empty — computed by the caller
	if meta.SHA256 != "" {
		t.Errorf("SHA256 should be empty, got %q", meta.SHA256)
	}
}

func TestDirectURLDownload_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	d := NewDirectURL()
	rc, meta, err := d.Download(context.Background(), srv.URL+"/missing.apk")
	if err == nil {
		rc.Close()
		t.Fatal("expected error for 404 response, got nil")
	}
	if meta != nil {
		t.Errorf("expected nil metadata on error, got %+v", meta)
	}
}

func TestDirectURLDownload_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewDirectURL()
	rc, _, err := d.Download(context.Background(), srv.URL+"/app.apk")
	if err == nil {
		rc.Close()
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestDirectURLDownload_InvalidURL(t *testing.T) {
	d := NewDirectURL()
	rc, _, err := d.Download(context.Background(), "://bad-url")
	if err == nil {
		rc.Close()
		t.Fatal("expected error for invalid URL, got nil")
	}
}

func TestDirectURLName(t *testing.T) {
	d := NewDirectURL()
	if got := d.Name(); got != "directurl" {
		t.Errorf("Name() = %q, want %q", got, "directurl")
	}
}

func TestDirectURLDownload_ContextCancelled(t *testing.T) {
	// Server that hangs — context should cancel before it responds
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	d := NewDirectURL()
	rc, _, err := d.Download(ctx, srv.URL+"/app.apk")
	if err == nil {
		rc.Close()
		t.Fatal("expected error for cancelled context, got nil")
	}
}
