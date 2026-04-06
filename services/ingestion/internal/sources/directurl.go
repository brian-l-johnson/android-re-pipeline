package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DirectURL downloads an APK directly from a provided URL.
type DirectURL struct {
	client *http.Client
}

// NewDirectURL creates a DirectURL source with a 5-minute timeout.
func NewDirectURL() *DirectURL {
	return &DirectURL{
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

// Name returns the source identifier.
func (d *DirectURL) Name() string {
	return "directurl"
}

// Download performs an HTTP GET on identifier (a URL) and streams the response
// body back to the caller. SHA256 is left empty — it is computed by the caller
// while writing to disk via io.TeeReader.
func (d *DirectURL) Download(ctx context.Context, identifier string) (io.ReadCloser, *APKMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, identifier, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("downloading APK: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("unexpected HTTP status %d from %s", resp.StatusCode, identifier)
	}

	meta := &APKMetadata{
		Source:      d.Name(),
		DownloadURL: identifier,
	}

	return resp.Body, meta, nil
}
