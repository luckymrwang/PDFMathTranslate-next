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
	order := PayOrder{ID: id, OpenID: "owner", State: "submitted", Engine: "Bing", LangIn: "en", LangOut: "zh",
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

	if err := s.pay.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openOrderStore(s.pay.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reloaded.db.Close() })
	s.pay.store = reloaded
	s.pay.referrals = &ReferralStore{db: reloaded.db}
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

func TestCancelledOrderHiddenButLatePaymentRevives(t *testing.T) {
	s := testPayServer(t)
	var paid sync.Mutex
	status := 0
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/stable_token" {
			writeJSON(w, 200, map[string]any{"access_token": "token", "expires_in": 7200})
			return
		}
		paid.Lock()
		defer paid.Unlock()
		if status == 0 {
			writeJSON(w, 200, map[string]any{"errcode": 268490004})
			return
		}
		writeJSON(w, 200, map[string]any{"errcode": 0, "order": map[string]any{"order_id": "Tcancel", "wx_order_id": "wx-9",
			"env_type": 1, "status": status, "order_fee": 50, "paid_fee": 50}})
	}))
	defer wx.Close()
	s.pay.client = &XPayClient{AppKey: "app-key", BaseURL: wx.URL, HTTP: wx.Client()}
	_ = s.pay.store.add(PayOrder{ID: "Tcancel", OpenID: "owner", State: "pending", Total: 50})

	w := httptest.NewRecorder()
	s.handlePayOrders(w, ownRequest("POST", "/pay/orders/Tcancel/cancel", "owner", nil))
	if order, _ := s.pay.store.get("Tcancel"); w.Code != 200 || order.State != "closed" {
		t.Fatalf("cancelled unpaid order not closed: %d %+v", w.Code, order)
	}
	w = httptest.NewRecorder()
	s.handlePayOrders(w, ownRequest("GET", "/pay/orders", "owner", nil))
	if strings.Contains(w.Body.String(), "Tcancel") {
		t.Fatal("unpaid closed order listed")
	}

	paid.Lock()
	status = 2
	paid.Unlock()
	if err := s.reconcileOrder(context.Background(), "Tcancel"); err != nil {
		t.Fatal(err)
	}
	if order, _ := s.pay.store.get("Tcancel"); order.State != "paid" {
		t.Fatalf("late payment not honoured: %+v", order)
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

func TestPointsSubmissionFailuresBoundedAndReturned(t *testing.T) {
	for _, mode := range []string{"rejected", "engine", "source"} {
		t.Run(mode, func(t *testing.T) {
			s := testPayServer(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); w.WriteHeader(422) }))
			defer upstream.Close()
			s.pdf2zh = NewPdf2zhClient(upstream.URL)
			o := paidOrderFixture(t, s, "submit-fail", "", 0)
			if err := s.pay.store.update(o.ID, func(o *PayOrder) error { o.State = "paid"; o.CreditPages = 2; o.CreditPoints = 20; return nil }); err != nil {
				t.Fatal(err)
			}
			if mode == "engine" {
				s.pay.store.update(o.ID, func(o *PayOrder) error { o.Engine = "missing"; return nil })
			}
			if mode == "source" {
				os.Remove(o.FilePath)
			}
			for i := 0; i < maxPaidAttempts+2; i++ {
				if _, err := s.startPaidOrder(context.Background(), o.ID); err == nil {
					t.Fatal("unexpected success")
				}
			}
			current, _ := s.pay.store.get(o.ID)
			if current.Delivery != "failed" || !current.CreditReturned || s.pay.referrals.credit("owner") != 20 {
				t.Fatal(current, s.pay.referrals.credit("owner"))
			}
		})
	}
}

func TestUncertainSubmissionReusesKeyUntilResolved(t *testing.T) {
	s := testPayServer(t)
	var keys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		keys = append(keys, r.Header.Get("X-Idempotency-Key"))
		if len(keys) <= maxPaidAttempts+1 {
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, map[string]string{"id": "possibly-existing-task"})
	}))
	defer upstream.Close()
	s.pdf2zh = NewPdf2zhClient(upstream.URL)
	o := paidOrderFixture(t, s, "uncertain", "", 0)
	for i := 0; i < maxPaidAttempts+1; i++ {
		if _, err := s.startPaidOrder(context.Background(), o.ID); err == nil {
			t.Fatal("expected uncertain error")
		}
	}
	current, _ := s.pay.store.get(o.ID)
	if current.Attempts != 0 || current.Delivery != "" {
		t.Fatal("uncertain task abandoned", current)
	}
	id, err := s.startPaidOrder(context.Background(), o.ID)
	if err != nil || id != "possibly-existing-task" {
		t.Fatal(id, err)
	}
	for _, key := range keys {
		if key != "virtualpay:uncertain" {
			t.Fatal("new task key after uncertain result", keys)
		}
	}
}

func TestFailedPointsUpgradeReplacementCanUpdate(t *testing.T) {
	s := testPayServer(t)
	old := PayOrder{ID: "failed-upgrade", OpenID: "owner", State: "paid", Engine: "Bing", CreditPages: 2, CreditPoints: 20, UpgradeFrom: "historical", TrialCredit: 3}
	if err := s.pay.store.add(old); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.update(old.ID, func(o *PayOrder) error { o.Delivery = "failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	replacement := PayOrder{ID: "replacement", OpenID: "owner", State: "pending", Engine: "Bing", UpgradeFrom: "historical", TrialCredit: 3}
	if err := s.pay.store.addPurchase(&replacement, 2, func(o *PayOrder, n int) error { return s.pricePurchase(o, n, "", "") }); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.update(replacement.ID, func(o *PayOrder) error { o.State = "submitted"; o.TaskID = "replacement-task"; return nil }); err != nil {
		t.Fatal("released upgrade blocked replacement", err)
	}
	if err := s.pay.store.update(replacement.ID, func(o *PayOrder) error { o.Delivery = "archived"; return nil }); err != nil {
		t.Fatal(err)
	}
}
