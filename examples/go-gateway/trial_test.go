package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFreeTrialIsOncePerUserAndTranslatesFirstThreePages(t *testing.T) {
	s := testPayServer(t)
	var forwarded map[string]any
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/engine/check":
			io.Copy(io.Discard, r.Body)
			writeJSON(w, 200, map[string]bool{"ready": true})
		case "/v1/translate":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			json.Unmarshal([]byte(r.FormValue("data")), &forwarded)
			writeJSON(w, 200, map[string]string{"id": "task-1"})
		default:
			t.Error("unexpected upstream", r.URL.Path)
		}
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")

	trial := func(owner string) *httptest.ResponseRecorder {
		s.prepared.Lock()
		s.prepared.files = map[string]preparedPDF{}
		s.prepared.Unlock()
		path := t.TempDir() + "/a.pdf"
		content := []byte("%PDF-test")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		s.prepared.Lock()
		s.prepared.files["f"] = preparedPDF{Owner: owner, Path: path, Name: "a.pdf", Hash: hex.EncodeToString(sum[:]),
			Pages: 30, Expires: time.Now().Add(time.Hour)}
		s.prepared.Unlock()
		// Paid engine and price are ignored for a trial.
		payload, _ := json.Marshal(map[string]any{"file_token": "f", "trial": true,
			"engine": "GPT-6", "lang_in": "en", "lang_out": "zh"})
		req := ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, req)
		return out
	}

	got := trial("owner")
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var created struct {
		Order   map[string]any    `json:"order"`
		PayData map[string]string `json:"payData"`
	}
	json.Unmarshal(got.Body.Bytes(), &created)
	if created.Order["state"] != "paid" || created.Order["trial"] != true ||
		created.Order["engine"] != trialEngine || created.Order["total_fen"].(float64) != 0 || created.PayData != nil || created.Order["pages"].(float64) != 3 || created.Order["document_pages"].(float64) != 30 {
		t.Fatal("unexpected trial order", got.Body.String())
	}
	if again := trial("owner"); again.Code != 409 {
		t.Fatal("second trial allowed", again.Code)
	}
	if other := trial("someone-else"); other.Code != 200 {
		t.Fatal("trial should be per user", other.Code)
	}

	id := created.Order["id"].(string)
	if _, err := s.startPaidOrder(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if forwarded["pages"] != "1-3" {
		t.Fatal("trial must translate only the first three pages:", forwarded)
	}
	if err := s.reconcileOrder(context.Background(), id); err != nil {
		t.Fatal("trial must not query WeChat:", err)
	}
}

func TestTrialOnlyModeCannotCreatePaidOrdersOrTranslateWholeDocument(t *testing.T) {
	s := testPayServer(t)
	s.pay.cfg.Enabled = false
	s.pay.cfg.TrialEnabled = true
	var forwarded map[string]any
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/engine/check":
			writeJSON(w, 200, map[string]bool{"ready": true})
		case "/v1/translate":
			r.ParseMultipartForm(1 << 20)
			json.Unmarshal([]byte(r.FormValue("data")), &forwarded)
			writeJSON(w, 200, map[string]string{"id": "trial-only-task"})
		default:
			t.Errorf("unexpected upstream %s", r.URL.Path)
		}
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	config := httptest.NewRecorder()
	s.handlePayConfig(config, ownRequest("GET", "/pay/config", "owner", nil))
	var cfg map[string]any
	json.Unmarshal(config.Body.Bytes(), &cfg)
	if cfg["enabled"] != false || cfg["trial_enabled"] != true || cfg["trial_pages"] != float64(3) {
		t.Fatal(config.Body.String())
	}
	for _, pages := range []int{1, 2, 3, 9} {
		t.Run(fmt.Sprint(pages), func(t *testing.T) {
			owner := fmt.Sprintf("owner-%d", pages)
			path := t.TempDir() + "/source.pdf"
			content := []byte("%PDF-test")
			os.WriteFile(path, content, 0600)
			hash := sha256.Sum256(content)
			s.prepared.Lock()
			if s.prepared.files == nil {
				s.prepared.files = make(map[string]preparedPDF)
			}
			s.prepared.files[owner] = preparedPDF{Owner: owner, Path: path, Name: "source.pdf", Hash: hex.EncodeToString(hash[:]), Pages: pages, Expires: time.Now().Add(time.Hour)}
			s.prepared.Unlock()
			payload, _ := json.Marshal(map[string]any{"file_token": owner, "trial": true, "engine": "GPT-6", "pages": "1-999", "lang_in": "en", "lang_out": "zh"})
			w := httptest.NewRecorder()
			// A normal authenticated user needs no payment-session signature for a trial.
			s.handlePreparedOrder(w, ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload)))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var created struct{ Order PayOrder }
			var body struct {
				Order map[string]any `json:"order"`
			}
			json.Unmarshal(w.Body.Bytes(), &body)
			id := body.Order["id"].(string)
			created.Order, _ = s.pay.store.get(id)
			limit := pages
			if limit > 3 {
				limit = 3
			}
			if created.Order.Pages != limit || created.Order.DocumentPages != pages || created.Order.Engine != "Bing" {
				t.Fatal(created.Order)
			}
			if _, err := s.startPaidOrder(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if forwarded["pages"] != fmt.Sprintf("1-%d", limit) || forwarded["only_include_translated_page"] != true {
				t.Fatal("page limit bypass", forwarded)
			}
		})
	}
	for _, prepared := range []bool{false, true} {
		w := httptest.NewRecorder()
		r := ownRequest("POST", "/pay/order", "owner", bytes.NewBufferString(`{"trial":false}`))
		if prepared {
			s.handlePreparedOrder(w, r)
		} else {
			s.handlePayOrder(w, r)
		}
		if w.Code != 503 {
			t.Fatal("paid creation must stay disabled", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.handleTranslateSubmit(w, ownRequest("POST", "/api/translate", "owner", nil))
	if w.Code != http.StatusPaymentRequired {
		t.Fatal("direct whole-document bypass", w.Code)
	}
}

func TestTrialUpgradePreservesFullSourcePageCount(t *testing.T) {
	s := testPayServer(t)
	raw := []byte("%PDF-original")
	path := t.TempDir() + "/source.pdf"
	os.WriteFile(path, raw, 0600)
	hash := sha256.Sum256(raw)
	// Upgrading must work before the periodic archiver has persisted results.
	order := PayOrder{ID: "trial-upgrade", OpenID: "owner", State: "submitted", Trial: true, Pages: 3, DocumentPages: 12, FilePath: path, FileHash: hex.EncodeToString(hash[:]), Name: "source.pdf"}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleRepreparePDF(w, ownRequest("POST", "/pay/prepare/order", "owner", bytes.NewBufferString(`{"order_id":"trial-upgrade"}`)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var prepared struct {
		Pages int `json:"page_count"`
	}
	json.Unmarshal(w.Body.Bytes(), &prepared)
	if prepared.Pages != 12 {
		t.Fatal("full quote must not charge for just trial pages", prepared)
	}
}

func TestTrialFailureDoesNotMentionPaymentOrRefund(t *testing.T) {
	for _, reason := range []string{"", alreadyTranslated} {
		if message := paidFailureMessage(PayOrder{Trial: true, FailReason: reason}); strings.Contains(message, "退款") {
			t.Fatal(message)
		}
	}
}

func TestTrialUpgradeChargesOnlyUntranslatedPagesOnce(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	content := []byte("%PDF-upgrade")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	trial := PayOrder{ID: "trial-1", OpenID: "owner", State: "submitted", Trial: true, Pages: 3, DocumentPages: 12, FileHash: hash}
	if err := s.pay.store.add(trial); err != nil {
		t.Fatal(err)
	}
	upgrade := func(owner, fileHash string) *httptest.ResponseRecorder {
		path := t.TempDir() + "/a.pdf"
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		s.prepared.Lock()
		s.prepared.files = map[string]preparedPDF{"f": {Owner: owner, Path: path, Name: "a.pdf", Hash: fileHash,
			Pages: 12, Expires: time.Now().Add(time.Hour)}}
		s.prepared.Unlock()
		payload, _ := json.Marshal(map[string]any{"file_token": "f", "upgrade_from": "trial-1",
			"engine": "Bing", "lang_in": "en", "lang_out": "zh"})
		req := ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, req)
		return out
	}
	if got := upgrade("owner", strings.Repeat("0", 64)); got.Code != 409 {
		t.Fatal("credit applied to a different file", got.Code)
	}
	if got := upgrade("stranger", hash); got.Code != 409 {
		t.Fatal("credit applied to another user's trial", got.Code)
	}
	got := upgrade("owner", hash)
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var created struct {
		Order map[string]any `json:"order"`
	}
	json.Unmarshal(got.Body.Bytes(), &created)
	// 12 pages translated, 3 already delivered by the trial, so 9 standard pages are billed.
	if created.Order["pages"].(float64) != 12 || created.Order["quantity"].(float64) != 9 ||
		created.Order["total_fen"].(float64) != 90 || created.Order["trial_credit_pages"].(float64) != 3 {
		t.Fatal("unexpected upgrade order", got.Body.String())
	}
	if again := upgrade("owner", hash); again.Code != 409 {
		t.Fatal("pending upgrade did not reserve trial credit", again.Code)
	}
	if err := s.pay.store.update(created.Order["id"].(string), func(o *PayOrder) error { o.State = "paid"; return nil }); err != nil {
		t.Fatal(err)
	}
	if again := upgrade("owner", hash); again.Code != 409 {
		t.Fatal("trial credited twice", again.Code)
	}
}

func TestTrialCreditReservationIsAtomicAndSurvivesRestart(t *testing.T) {
	s := testPayServer(t)
	trial := PayOrder{ID: "trial-reserve", OpenID: "owner", Trial: true, Pages: 3, FileHash: "hash", State: "submitted"}
	if err := s.pay.store.add(trial); err != nil {
		t.Fatal(err)
	}
	first := PayOrder{ID: "upgrade-a", OpenID: "owner", UpgradeFrom: trial.ID, TrialCredit: 3, State: "pending"}
	second := first
	second.ID = "upgrade-b"
	// Both callers can have obtained a quote; add must still enforce uniqueness.
	if _, ok := s.pay.store.trialCredit(trial.ID, "owner", "hash"); !ok {
		t.Fatal("missing credit")
	}
	if err := s.pay.store.add(first); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.add(second); err == nil {
		t.Fatal("second pending reservation accepted")
	}
	if err := s.pay.store.update(first.ID, func(o *PayOrder) error { o.State = "closed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openOrderStore(s.pay.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.db.Close() })
	s.pay.store = reopened
	s.pay.referrals = &ReferralStore{db: reopened.db}
	if _, ok := reopened.trialCredit(trial.ID, "owner", "hash"); ok {
		t.Fatal("local cancellation released trial credit")
	}
	if err := reopened.update(first.ID, func(o *PayOrder) error { o.PaymentClosed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.trialCredit(trial.ID, "owner", "hash"); !ok {
		t.Fatal("confirmed closure did not release reservation")
	}
	if err := reopened.add(second); err != nil {
		t.Fatal(err)
	}
	s.pay.store = reopened
	if err := s.grantOrder(first.ID, queriedOrder{ID: first.ID, EnvType: 1, Status: 2, WxID: "late-upgrade"}); err == nil {
		t.Fatal("old upgrade revived after reservation was released")
	}
}

func TestLegacyPendingUpgradesCannotBothConsumeTrialCredit(t *testing.T) {
	s := testPayServer(t)
	// Simulate two pending records persisted before unique reservations existed.
	for _, id := range []string{"legacy-a", "legacy-b"} {
		if err := s.pay.store.saveLocked(PayOrder{ID: id, OpenID: "owner", UpgradeFrom: "trial", TrialCredit: 3, State: "pending", Total: 90}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"legacy-a", "legacy-b"} {
		err := s.grantOrder(id, queriedOrder{ID: id, EnvType: 1, Status: 2, WxID: "wx-" + id, OrderFee: 90, PaidFee: 90})
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && err == nil {
			t.Fatal("legacy trial credit consumed twice")
		}
	}
}
