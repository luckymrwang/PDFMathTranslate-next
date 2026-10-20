package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHandleOversizeLogsOnlyOversizedSizes(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	srv := &Server{}
	call := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/oversize", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyOpenID, "openid-1"))
		rec := httptest.NewRecorder()
		srv.handleOversize(rec, req)
		return rec.Code
	}
	if code := call(`{"size": 41943040}`); code != http.StatusNoContent {
		t.Fatalf("oversize report status = %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "oversize_pdf source=client size_mb=40.0 user=") || strings.Contains(out, "openid-1") {
		t.Fatalf("unexpected log line: %q", out)
	}
	if code := call(`{"size": 1000}`); code != http.StatusBadRequest {
		t.Fatalf("in-limit size must be rejected, got %d", code)
	}
	if code := call(`not json`); code != http.StatusBadRequest {
		t.Fatalf("invalid body must be rejected, got %d", code)
	}
}
