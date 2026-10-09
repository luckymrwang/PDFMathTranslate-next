package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func paidOrderFixture(t *testing.T, s *Server, id, taskID string, attempts int) PayOrder {
	t.Helper()
	path := filepath.Join(s.pay.store.dir, "uploads", id+".pdf")
	content := []byte("%PDF-1.4 test")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	order := PayOrder{ID: id, OpenID: "owner", State: "submitted", Engine: "Google", LangIn: "en", LangOut: "zh",
		FilePath: path, FileHash: hex.EncodeToString(sum[:]), Name: "a.pdf", TaskID: taskID, Attempts: attempts}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	s.tasks.set(taskID, "owner")
	return order
}

func TestPaidResultSurvivesWorkerLoss(t *testing.T) {
	s := testPayServer(t)
	var mu sync.Mutex
	var keys []string
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/translate":
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			keys = append(keys, r.Header.Get("X-Idempotency-Key"))
			mu.Unlock()
			writeJSON(w, 200, map[string]string{"id": "task-2"})
		case r.URL.Path == "/v1/translate/task-2" && r.Method == http.MethodGet:
			writeJSON(w, 200, map[string]any{"state": "finished"})
		case r.URL.Path == "/v1/translate/task-2/mono":
			_, _ = io.WriteString(w, "%PDF-mono")
		default:
			// task-1 was lost by a worker restart; dual output is disabled.
			http.NotFound(w, r)
		}
	}))
	s.pdf2zh = NewPdf2zhClient(python.URL)
	order := paidOrderFixture(t, s, "Tpaid1", "task-1", 0)

	if err := s.advancePaidOrder(context.Background(), order.ID, true); err != nil {
		t.Fatal(err)
	}
	retried, _ := s.pay.store.get(order.ID)
	if retried.TaskID != "task-2" || retried.Attempts != 1 || len(keys) != 1 || keys[0] != "virtualpay:Tpaid1:1" {
		t.Fatalf("lost task not resubmitted under a new key: %+v %v", retried, keys)
	}
	if err := s.advancePaidOrder(context.Background(), order.ID, true); err != nil {
		t.Fatal(err)
	}
	python.Close()

	reloaded, err := openOrderStore(s.pay.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	done, _ := reloaded.get(order.ID)
	if done.Delivery != "archived" || done.MonoFile == "" || done.DualFile != "" {
		t.Fatalf("result not durably archived: %+v", done)
	}

	// The worker is gone; both the old and new task ids are served from the archive.
	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-1", "owner", nil))
	var status map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if w.Code != 200 || status["state"] != "finished" || status["mono_url"] == nil {
		t.Fatalf("archived status not served: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-2/mono", "owner", nil))
	if w.Code != 200 || w.Body.String() != "%PDF-mono" {
		t.Fatalf("archived PDF not served: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-2/mono", "other", nil))
	if w.Code != 404 {
		t.Fatal("archived PDF exposed to another account")
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("DELETE", "/api/translate/task-2", "owner", nil))
	if w.Code != http.StatusConflict {
		t.Fatal("paid task must not be cancellable by the client")
	}
}

func TestPaidOrderFailsAfterMaxAttempts(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("exhausted order must not be resubmitted")
		}
		writeJSON(w, 200, map[string]any{"state": "error"})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	order := paidOrderFixture(t, s, "Tpaid2", "task-9", maxPaidAttempts-1)

	if err := s.advancePaidOrder(context.Background(), order.ID, true); err != errPaidFailed {
		t.Fatalf("expected permanent failure, got %v", err)
	}
	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/task-9", "owner", nil))
	if !strings.Contains(w.Body.String(), `"state":"error"`) {
		t.Fatalf("failed order not reported: %s", w.Body.String())
	}
}

func TestOrderLocksAreIndependent(t *testing.T) {
	var locks orderLocks
	unlockA, _ := locks.acquire("a", true)
	if _, ok := locks.acquire("a", false); ok {
		t.Fatal("same order locked twice")
	}
	unlockB, ok := locks.acquire("b", false)
	if !ok {
		t.Fatal("a busy order blocked another order")
	}
	unlockB()
	unlockA()
	if len(locks.m) != 0 {
		t.Fatal("order locks leaked")
	}
}
