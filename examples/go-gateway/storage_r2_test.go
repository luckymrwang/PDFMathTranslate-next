package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadR2Config(t *testing.T) {
	account := strings.Repeat("a1", 16)
	t.Setenv("R2_ACCOUNT_ID", account)
	t.Setenv("R2_BUCKET", "pdf-results")
	t.Setenv("R2_ACCESS_KEY_ID", "AK")
	t.Setenv("R2_SECRET_ACCESS_KEY", "SK")
	t.Setenv("OSS_ENDPOINT", "https://ignored.example.com")
	cfg, ok := LoadS3Config()
	if !ok || cfg.Provider != "r2" || cfg.Endpoint != "https://"+account+".r2.cloudflarestorage.com" ||
		cfg.Region != "auto" || !cfg.PathStyle || !cfg.Proxy {
		t.Fatalf("unexpected R2 config: %+v", cfg)
	}
	t.Setenv("R2_JURISDICTION", "eu")
	if cfg, _ := LoadS3Config(); cfg.host != account+".eu.r2.cloudflarestorage.com" {
		t.Fatalf("jurisdiction endpoint not applied: %s", cfg.host)
	}
	t.Setenv("R2_ACCOUNT_ID", "evil.com/x")
	if cfg, ok := LoadS3Config(); ok && cfg.Provider == "r2" {
		t.Fatal("malformed account id accepted")
	}
}

func TestPaidResultMovesToObjectStorage(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			objects[r.URL.Path], _ = io.ReadAll(r.Body)
		case http.MethodGet:
			if r.URL.Query().Get("X-Amz-Signature") == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			body, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		}
	}))
	defer bucket.Close()
	endpoint, _ := url.Parse(bucket.URL)

	s := testPayServer(t)
	s.storage = NewS3Client(&S3Config{Endpoint: bucket.URL, Region: "auto", Bucket: "pdf-results", AccessKey: "AK",
		SecretKey: "SK", PathStyle: true, URLTTL: time.Hour, Proxy: true, Provider: "r2", host: endpoint.Host, scheme: "http"})
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/translate/task-1":
			writeJSON(w, 200, map[string]any{"state": "finished"})
		case "/v1/translate/task-1/dual":
			_, _ = io.WriteString(w, "%PDF-dual")
		default:
			http.NotFound(w, r)
		}
	}))
	s.pdf2zh = NewPdf2zhClient(python.URL)
	order := paidOrderFixture(t, s, "Tr2", "task-1", 0)

	if err := s.advancePaidOrder(context.Background(), order.ID, true); err != nil {
		t.Fatal(err)
	}
	python.Close()
	done, _ := s.pay.store.get(order.ID)
	if done.Delivery != "archived" || done.DualKey != "results/owner/Tr2-dual.pdf" || done.MonoKey != "" || done.DualFile != "" {
		t.Fatalf("result not moved to object storage: %+v", done)
	}
	if _, err := os.Stat(filepath.Join(s.pay.store.dir, "results", "Tr2-dual.pdf")); err == nil {
		t.Fatal("local copy kept after upload")
	}

	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-1/result", "owner", nil))
	if w.Code != http.StatusNotImplemented {
		t.Fatal("proxy mode must not hand out storage URLs")
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-1/dual", "owner", nil))
	if w.Code != 200 || w.Body.String() != "%PDF-dual" || w.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("result not streamed from object storage: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-1/mono", "owner", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("missing variant must be 404")
	}
}
