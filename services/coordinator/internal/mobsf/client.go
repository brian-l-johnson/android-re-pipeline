package mobsf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	pollInterval        = 10 * time.Second
	statusCheckInterval = 2 * time.Minute // how often to probe /api/v1/scans
	uploadTimeout       = 5 * time.Minute
	apiTimeout          = 30 * time.Second
	scanSearchPages     = 5  // max pages to search when looking for a hash
	scanPageSize        = 10 // entries per page
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

// ScanStatus describes the current state of a MobSF scan as reported by
// /api/v1/scans. Found=false means the hash was not present in the scan list.
type ScanStatus struct {
	Found    bool
	HasError bool // true if MobSF logged an exception for this scan
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

// PollScan kicks off a scan for scanHash and polls until MobSF reports
// completion. No wall-clock timeout is imposed; instead, every
// statusCheckInterval the /api/v1/scans endpoint is queried to check whether
// MobSF itself considers the scan failed. An error is returned only if:
//   - the context is cancelled
//   - MobSF logs an exception for the scan
//   - the scan disappears from MobSF's scan list entirely
func (c *Client) PollScan(ctx context.Context, scanHash string) (*ScanResult, error) {
	if err := c.startScan(ctx, scanHash); err != nil {
		return nil, fmt.Errorf("start scan: %w", err)
	}

	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	statusTicker := time.NewTicker(statusCheckInterval)
	defer statusTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-pollTicker.C:
			report, done, err := c.tryGetReport(ctx, scanHash)
			if err != nil {
				// Transient network/API error — keep polling.
				log.Printf("mobsf: transient poll error (hash=%s): %v", scanHash, err)
				continue
			}
			if done {
				return &ScanResult{Report: report}, nil
			}

		case <-statusTicker.C:
			status, err := c.GetScanStatus(ctx, scanHash)
			if err != nil {
				// Transient — don't fail the scan over a status check blip.
				log.Printf("mobsf: status check error (hash=%s): %v", scanHash, err)
				continue
			}
			if !status.Found {
				return nil, fmt.Errorf("scan %s not found in MobSF scan list (deleted?)", scanHash)
			}
			if status.HasError {
				return nil, fmt.Errorf("MobSF reported a scan failure for hash %s", scanHash)
			}
			log.Printf("mobsf: scan %s still in progress", scanHash)
		}
	}
}

// GetScanStatus queries /api/v1/scans (newest-first) for scanHash and returns
// its current status. It searches up to scanSearchPages pages.
//
// MobSF stores SCAN_LOGS as a Python-formatted list of dicts, e.g.
//
//	[{'timestamp': '...', 'status': 'Generating Hashes', 'exception': None}, ...]
//
// A scan has an error if any log entry has a non-None exception value.
func (c *Client) GetScanStatus(ctx context.Context, scanHash string) (*ScanStatus, error) {
	for page := 1; page <= scanSearchPages; page++ {
		apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
		url := fmt.Sprintf("%s/api/v1/scans?page=%d&page_size=%d", c.baseURL, page, scanPageSize)
		req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("build scans request: %w", err)
		}
		req.Header.Set("Authorization", c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("scans request: %w", err)
		}

		var payload struct {
			Content []struct {
				MD5      string `json:"MD5"`
				ScanLogs string `json:"SCAN_LOGS"`
			} `json:"content"`
			NumPages int `json:"num_pages"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		cancel()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode scans response: %w", decodeErr)
		}

		for _, entry := range payload.Content {
			if entry.MD5 != scanHash {
				continue
			}
			// Detect real exceptions: count total 'exception': occurrences vs
			// 'exception': None occurrences. If they differ, at least one entry
			// has a non-None exception.
			total := strings.Count(entry.ScanLogs, "'exception':")
			nones := strings.Count(entry.ScanLogs, "'exception': None")
			return &ScanStatus{
				Found:    true,
				HasError: total > 0 && total != nones,
			}, nil
		}

		if page >= payload.NumPages {
			break
		}
	}
	return &ScanStatus{Found: false}, nil
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

	// MobSF returns 404 while the scan is still running.
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

// CheckReport fetches the report for scanHash if MobSF already has it,
// without starting a new scan. Returns (result, true, nil) when found,
// (nil, false, nil) when not yet available, or (nil, false, err) on error.
func (c *Client) CheckReport(ctx context.Context, scanHash string) (*ScanResult, bool, error) {
	report, done, err := c.tryGetReport(ctx, scanHash)
	if err != nil {
		return nil, false, err
	}
	if !done {
		return nil, false, nil
	}
	return &ScanResult{Report: report}, true, nil
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
