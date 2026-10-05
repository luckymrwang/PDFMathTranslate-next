package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Pdf2zhClient is a thin proxy client for the Python translation API
// (pdf2zh_next/http_api.py). The gateway forwards authenticated requests to it.
type Pdf2zhClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewPdf2zhClient returns a client for the given base URL.
func NewPdf2zhClient(baseURL string) *Pdf2zhClient {
	return &Pdf2zhClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// No overall timeout: SSE streams can run for many minutes.
		HTTP: &http.Client{},
	}
}

// Submit forwards a multipart upload (file + data) and returns the task id.
func (c *Pdf2zhClient) Submit(ctx context.Context, contentType string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/translate", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Request issues a GET/DELETE to an upstream task path (e.g. "/v1/translate/<id>")
// and returns the raw response for the caller to stream back to the client.
func (c *Pdf2zhClient) Request(ctx context.Context, method, upstreamPath string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+upstreamPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	return c.HTTP.Do(req)
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("upstream error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// defaultSubmitTimeout caps how long the initial submit (upload) may take.
const defaultSubmitTimeout = 2 * time.Minute
