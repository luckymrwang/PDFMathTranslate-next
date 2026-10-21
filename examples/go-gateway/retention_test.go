package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExpiredOrderFilesAreDeletedAfterRetention(t *testing.T) {
	t.Setenv("RESULT_RETENTION_HOURS", "72")
	var mu sync.Mutex
	var deleted []string
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodDelete || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		deleted = append(deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer bucket.Close()
	endpoint, _ := url.Parse(bucket.URL)

	s := testPayServer(t)
	s.storage = NewS3Client(&S3Config{Endpoint: bucket.URL, Region: "auto", Bucket: "pdf-results", AccessKey: "AK",
		SecretKey: "SK", PathStyle: true, URLTTL: time.Hour, Provider: "r2", host: endpoint.Host, scheme: "http"})

	fresh := paidOrderFixture(t, s, "Tfresh", "task-f", 0)
	old := paidOrderFixture(t, s, "Told", "task-o", 0)
	for _, id := range []string{fresh.ID, old.ID} {
		created := time.Now().Add(-time.Hour).Unix()
		if id == old.ID {
			created = time.Now().Add(-73 * time.Hour).Unix()
		}
		if err := s.pay.store.update(id, func(o *PayOrder) error {
			o.Delivery, o.DualKey, o.CreatedAt = "archived", "results/owner/"+id+"-dual.pdf", created
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{fresh.ID, old.ID} {
		if err := s.expireOrder(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}

	kept, _ := s.pay.store.get(fresh.ID)
	if kept.Delivery != "archived" || kept.DualKey == "" || kept.FilePath == "" {
		t.Fatalf("order inside retention was cleaned: %+v", kept)
	}
	gone, _ := s.pay.store.get(old.ID)
	if gone.Delivery != "expired" || gone.DualKey != "" || gone.FilePath != "" {
		t.Fatalf("expired order not cleaned: %+v", gone)
	}
	if _, err := os.Stat(old.FilePath); err == nil {
		t.Fatal("uploaded PDF kept after retention")
	}
	if _, err := os.Stat(fresh.FilePath); err != nil {
		t.Fatal("uploaded PDF removed too early")
	}
	if len(deleted) != 1 || deleted[0] != "/pdf-results/results/owner/Told-dual.pdf" {
		t.Fatalf("unexpected object deletions: %v", deleted)
	}

	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-o/dual", "owner", nil))
	if w.Code != http.StatusGone {
		t.Fatal("expired result must return 410, got", w.Code)
	}
}

func TestInProgressOrderIsNeverExpired(t *testing.T) {
	s := testPayServer(t)
	order := paidOrderFixture(t, s, "Tbusy", "task-b", 0)
	if err := s.pay.store.update(order.ID, func(o *PayOrder) error {
		o.CreatedAt = time.Now().Add(-100 * time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.expireOrder(context.Background(), order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(order.FilePath); err != nil {
		t.Fatal("input of a translation in progress was deleted")
	}
}

func TestDownloadAfterBucketLifecycleDeleteReturnsGone(t *testing.T) {
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer bucket.Close()
	endpoint, _ := url.Parse(bucket.URL)
	s := testPayServer(t)
	s.storage = NewS3Client(&S3Config{Endpoint: bucket.URL, Region: "auto", Bucket: "pdf-results", AccessKey: "AK",
		SecretKey: "SK", PathStyle: true, URLTTL: time.Hour, Proxy: true, Provider: "r2", host: endpoint.Host, scheme: "http"})
	order := paidOrderFixture(t, s, "Tgone", "task-g", 0)
	if err := s.pay.store.update(order.ID, func(o *PayOrder) error {
		o.Delivery, o.DualKey = "archived", "results/owner/Tgone-dual.pdf"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-g/dual", "owner", nil))
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "重新下单") {
		t.Fatalf("expected friendly 410, got %d %s", w.Code, w.Body.String())
	}
}
