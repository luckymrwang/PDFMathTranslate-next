// Package main is a minimal, dependency-free Go client for the pdf2zh_next
// HTTP API (see pdf2zh_next/http_api.py). It covers the full flow a gateway
// needs: submit a PDF, follow progress via SSE or polling, and download the
// translated results.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to a pdf2zh_next HTTP API instance.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client for the given base URL (e.g. http://127.0.0.1:11008).
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// No overall timeout: translation streams can run for many minutes.
		HTTP: &http.Client{},
	}
}

// TranslateRequest mirrors the `data` JSON accepted by POST /v1/translate.
type TranslateRequest struct {
	LangIn              string         `json:"lang_in,omitempty"`
	LangOut             string         `json:"lang_out,omitempty"`
	QPS                 int            `json:"qps,omitempty"`
	Pages               string         `json:"pages,omitempty"`
	NoMono              bool           `json:"no_mono,omitempty"`
	NoDual              bool           `json:"no_dual,omitempty"`
	WatermarkOutputMode string         `json:"watermark_output_mode,omitempty"`
	EngineSettings      map[string]any `json:"translate_engine_settings,omitempty"`
}

// Event is a single progress/finish/error event from the server.
type Event struct {
	Type            string         `json:"type"`
	Stage           string         `json:"stage,omitempty"`
	OverallProgress float64        `json:"overall_progress,omitempty"`
	PartIndex       int            `json:"part_index,omitempty"`
	TotalParts      int            `json:"total_parts,omitempty"`
	StageCurrent    int            `json:"stage_current,omitempty"`
	StageTotal      int            `json:"stage_total,omitempty"`
	TotalSeconds    float64        `json:"total_seconds,omitempty"`
	MonoURL         string         `json:"mono_url,omitempty"`
	DualURL         string         `json:"dual_url,omitempty"`
	TokenUsage      map[string]any `json:"token_usage,omitempty"`
	Error           string         `json:"error,omitempty"`
	ErrorType       string         `json:"error_type,omitempty"`
	Details         string         `json:"details,omitempty"`
}

// Status is the response of GET /v1/translate/{id}.
type Status struct {
	ID           string         `json:"id"`
	State        string         `json:"state"`
	Stage        string         `json:"stage"`
	Progress     float64        `json:"progress"`
	Error        string         `json:"error"`
	ErrorType    string         `json:"error_type"`
	TotalSeconds float64        `json:"total_seconds"`
	TokenUsage   map[string]any `json:"token_usage"`
	MonoURL      string         `json:"mono_url"`
	DualURL      string         `json:"dual_url"`
}

// Submit uploads a PDF and returns the task id.
func (c *Client) Submit(ctx context.Context, pdfPath string, req TranslateRequest) (string, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	fw, err := mw.CreateFormFile("file", filepath.Base(pdfPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}

	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if err := mw.WriteField("data", string(data)); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/translate", &body)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.HTTP.Do(httpReq)
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

// Stream consumes the SSE progress stream, invoking handler for each event.
// It returns when the stream ends (after a finish or error event) or ctx is done.
func (c *Client) Stream(ctx context.Context, taskID string, handler func(Event)) error {
	url := fmt.Sprintf("%s/v1/translate/%s/stream", c.BaseURL, taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil // ignore comments/keep-alives that aren't JSON
		}
		handler(ev)
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "": // event boundary
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// ignore other SSE fields (event:, id:, :comment)
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

// Status fetches the current task status (for polling clients).
func (c *Client) Status(ctx context.Context, taskID string) (*Status, error) {
	url := fmt.Sprintf("%s/v1/translate/%s", c.BaseURL, taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Poll repeatedly calls Status until the task is finished/error/cancelled,
// invoking onUpdate on each change. Useful for clients without SSE support.
func (c *Client) Poll(ctx context.Context, taskID string, interval time.Duration, onUpdate func(*Status)) (*Status, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		st, err := c.Status(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if onUpdate != nil {
			onUpdate(st)
		}
		switch st.State {
		case "finished", "error", "cancelled":
			return st, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Download saves a result PDF. kind must be "mono" or "dual".
func (c *Client) Download(ctx context.Context, taskID, kind, destPath string) error {
	url := fmt.Sprintf("%s/v1/translate/%s/%s", c.BaseURL, taskID, kind)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// Cancel aborts a running task and asks the server to clean up.
func (c *Client) Cancel(ctx context.Context, taskID string) error {
	url := fmt.Sprintf("%s/v1/translate/%s", c.BaseURL, taskID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("api error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}
