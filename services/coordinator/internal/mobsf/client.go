package mobsf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	pollInterval  = 10 * time.Second
	scanTimeout   = 10 * time.Minute
	uploadTimeout = 5 * time.Minute
	apiTimeout    = 30 * time.Second
)

// Client calls the MobSF REST API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient creates a MobSF client pointed at baseURL with the given API key.
// The underlying http.Client has no global timeout; each method applies its
// own context deadline so that large APK uploads get a generous window while
// short API calls remain bounded.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{},
	}
}

// ScanResult holds the raw JSON report returned by MobSF.
type ScanResult struct {
	Report json.RawMessage
}

// Upload sends an APK file to MobSF and returns the scan hash.
func (c *Client) Upload(ctx context.Context, apkPath string) (string, error) {
	f, err := os.Open(apkPath)
	if err != nil {
		return "", fmt.Errorf("open apk: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	part, err := mw.CreateFormFile("file", filepath.Base(apkPath))
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("copy apk to form: %w", err)
	}
	mw.Close()

	uploadCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPost, c.baseURL+"/api/v1/upload", &body)
	if err != nil {
		return "", fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("upload failed (status %d): %s", resp.StatusCode, raw)
	}

	var result struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode upload response: %w", err)
	}
	if result.Hash == "" {
		return "", fmt.Errorf("upload response missing hash")
	}
	return result.Hash, nil
}

// PollScan kicks off a scan for scanHash and polls until completion or timeout.
func (c *Client) PollScan(ctx context.Context, scanHash string) (*ScanResult, error) {
	// Kick off the scan
	if err := c.startScan(ctx, scanHash); err != nil {
		return nil, fmt.Errorf("start scan: %w", err)
	}

	deadline := time.Now().Add(scanTimeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case t := <-ticker.C:
			if t.After(deadline) {
				return nil, fmt.Errorf("scan timed out after %s", scanTimeout)
			}

			report, done, err := c.tryGetReport(ctx, scanHash)
			if err != nil {
				// Transient error — keep polling
				continue
			}
			if done {
				return &ScanResult{Report: report}, nil
			}
		}
	}
}

func (c *Client) startScan(ctx context.Context, scanHash string) error {
	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	formData := fmt.Sprintf("hash=%s", scanHash)
	req, err := http.NewRequestWithContext(apiCtx, http.MethodPost, c.baseURL+"/api/v1/scan",
		bytes.NewBufferString(formData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("scan start failed (status %d): %s", resp.StatusCode, raw)
	}
	return nil
}

// tryGetReport attempts to fetch the JSON report for scanHash.
// done is true when the report is available.
func (c *Client) tryGetReport(ctx context.Context, scanHash string) (json.RawMessage, bool, error) {
	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	formData := fmt.Sprintf("hash=%s", scanHash)
	req, err := http.NewRequestWithContext(apiCtx, http.MethodPost, c.baseURL+"/api/v1/report_json",
		bytes.NewBufferString(formData))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	// MobSF returns 404 while the scan is still running
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("report_json returned status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(raw), true, nil
}

// GetScorecard fetches the MobSF scorecard for scanHash.
func (c *Client) GetScorecard(ctx context.Context, scanHash string) (json.RawMessage, error) {
	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/api/v1/scorecard?hash=%s", c.baseURL, scanHash)
	req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build scorecard request: %w", err)
	}
	req.Header.Set("Authorization", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scorecard request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("scorecard failed (status %d): %s", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read scorecard response: %w", err)
	}
	return json.RawMessage(raw), nil
}
