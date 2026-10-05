package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArchiveResultFlow(t *testing.T) {
	// Mock Python translation API.
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/translate/task123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"finished"}`))
		case "/v1/translate/task123/mono":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-mono"))
		case "/v1/translate/task123/dual":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-dual"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer python.Close()

	// Mock S3-compatible store; record PUTs.
	var mu sync.Mutex
	var puts []string
	var auths []string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			mu.Lock()
			puts = append(puts, r.URL.Path)
			auths = append(auths, r.Header.Get("Authorization"))
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()

	// Configure storage via env, then load.
	os.Setenv("OSS_ENDPOINT", s3.URL)
	os.Setenv("OSS_BUCKET", "mybucket")
	os.Setenv("OSS_ACCESS_KEY", "AK")
	os.Setenv("OSS_SECRET_KEY", "SK")
	os.Setenv("OSS_PATH_STYLE", "true")
	os.Setenv("OSS_REGION", "us-east-1")
	defer func() {
		for _, k := range []string{"OSS_ENDPOINT", "OSS_BUCKET", "OSS_ACCESS_KEY", "OSS_SECRET_KEY", "OSS_PATH_STYLE", "OSS_REGION"} {
			os.Unsetenv(k)
		}
	}()

	s3cfg, ok := LoadS3Config()
	if !ok {
		t.Fatal("LoadS3Config returned not-ok")
	}
	s3cfg.URLTTL = time.Hour

	srv := &Server{
		pdf2zh:  NewPdf2zhClient(python.URL),
		tasks:   newTaskRegistry(),
		storage: NewS3Client(s3cfg),
	}
	srv.tasks.set("task123", "openidA")

	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/translate/task123/result", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyOpenID, "openidA"))
		srv.handleTranslateResult(rec, req, "task123")
		return rec
	}

	rec := call()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	monoURL, _ := out["mono_url"].(string)
	dualURL, _ := out["dual_url"].(string)
	for _, u := range []string{monoURL, dualURL} {
		if !strings.HasPrefix(u, "http") ||
			!strings.Contains(u, "X-Amz-Signature=") ||
			!strings.Contains(u, "X-Amz-Credential=") {
			t.Fatalf("bad presigned url: %q", u)
		}
	}

	mu.Lock()
	gotPuts := append([]string(nil), puts...)
	gotAuths := append([]string(nil), auths...)
	mu.Unlock()

	if len(gotPuts) != 2 {
		t.Fatalf("expected 2 PUTs, got %d: %v", len(gotPuts), gotPuts)
	}
	wantPaths := map[string]bool{
		"/mybucket/results/openidA/task123-mono.pdf": false,
		"/mybucket/results/openidA/task123-dual.pdf": false,
	}
	for _, p := range gotPuts {
		if _, known := wantPaths[p]; !known {
			t.Fatalf("unexpected PUT path: %s", p)
		}
		wantPaths[p] = true
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Fatalf("missing PUT for %s", p)
		}
	}
	for _, a := range gotAuths {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AK/") {
			t.Fatalf("bad authorization header: %q", a)
		}
	}

	// Second call must be cached (no new PUTs).
	rec2 := call()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call expected 200, got %d", rec2.Code)
	}
	mu.Lock()
	putCount := len(puts)
	mu.Unlock()
	if putCount != 2 {
		t.Fatalf("expected PUT count to stay 2 after cached call, got %d", putCount)
	}
}
