package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestFirstLoginRewardsAndBingPointsPurchases(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	order := func(owner string, body map[string]any) map[string]any {
		path := t.TempDir() + "/a.pdf"
		os.WriteFile(path, []byte("%PDF-"+owner), 0600)
		s.prepared.Lock()
		s.prepared.files = map[string]preparedPDF{"f": {Owner: owner, Path: path, Name: "a.pdf", Hash: owner,
			Pages: 12, Expires: time.Now().Add(time.Hour)}}
		s.prepared.Unlock()
		body["file_token"], body["lang_in"], body["lang_out"] = "f", "en", "zh"
		payload, _ := json.Marshal(body)
		req := ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		s.handlePreparedOrder(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	w := httptest.NewRecorder()
	s.handleReferral(w, ownRequest("GET", "/pay/referral", "inviter", nil))
	var view struct {
		Code string `json:"code"`
	}
	json.Unmarshal(w.Body.Bytes(), &view)
	if view.Code == "" {
		t.Fatal("missing invite code", w.Body.String())
	}
	if err := s.pay.referrals.login("friend", view.Code, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.pay.referrals.credit("inviter") != 100 || s.pay.referrals.credit("friend") != 100 {
		t.Fatal("first login rewards missing")
	}
	// Credit only covers standard translation; a fully covered order needs no payment.
	if _, err := s.pay.referrals.db.Exec("UPDATE referral_users SET credit=150 WHERE openid='inviter'"); err != nil {
		t.Fatal(err)
	}
	free := order("inviter", map[string]any{"engine": "Bing"})
	if free["payData"] != nil || free["order"].(map[string]any)["state"] != "paid" ||
		free["order"].(map[string]any)["credit_pages"].(float64) != 12 || free["order"].(map[string]any)["total_fen"].(float64) != 0 {
		t.Fatal("unexpected free order", free)
	}
	partial := order("inviter", map[string]any{"engine": "Bing"})["order"].(map[string]any)
	if partial["credit_pages"].(float64) != 3 || partial["quantity"].(float64) != 9 || partial["total_fen"].(float64) != 90 {
		t.Fatal("unexpected partially credited order", partial)
	}
	if s.pay.referrals.credit("inviter") != 0 {
		t.Fatal("credit not reserved", s.pay.referrals.credit("inviter"))
	}
	// An unpaid order gives its pages back exactly once.
	s.pay.store.update(partial["id"].(string), func(o *PayOrder) error { o.State, o.PaymentClosed = "closed", true; return nil })
	for i := 0; i < 2; i++ {
		current, _ := s.pay.store.get(partial["id"].(string))
		s.settleReferrals(current)
	}
	if got := s.pay.referrals.credit("inviter"); got != 30 {
		t.Fatal("closed order credit not returned once", got)
	}
	// Reopening the store keeps codes and balances.
	reopened, err := openReferralStore(s.pay.store.dir)
	if err == nil {
		defer reopened.db.Close()
	}
	if err != nil || reopened.credit("inviter") != 30 || reopened.inviter(view.Code, "someone") != "inviter" {
		t.Fatal("referrals not persisted", err)
	}
}

func TestReferralLocalCancellationKeepsCreditForLatePayment(t *testing.T) {
	s := testPayServer(t)
	if _, err := s.pay.referrals.view("owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.referrals.restore("owner", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pay.referrals.reserve("owner", 3); err != nil {
		t.Fatal(err)
	}
	order := PayOrder{ID: "late-credit", OpenID: "owner", State: "closed", CreditPages: 3, CreditPoints: 3, Total: 90}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	s.settleReferrals(order)
	if got := s.pay.referrals.credit("owner"); got != 2 {
		t.Fatal("local cancellation released spendable credit", got)
	}
	if err := s.grantOrder(order.ID, queriedOrder{ID: order.ID, EnvType: 1, Status: 2, WxID: "late-wx", OrderFee: 90, PaidFee: 90}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.pay.store.get(order.ID)
	if current.State != "paid" || current.CreditReturned {
		t.Fatal(current)
	}
}

func TestReferralReturnSurvivesWriteFailuresAndRestart(t *testing.T) {
	s := testPayServer(t)
	if err := s.pay.referrals.restore("owner", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pay.referrals.reserve("owner", 3); err != nil {
		t.Fatal(err)
	}
	order := PayOrder{ID: "return-credit", OpenID: "owner", State: "pending", CreditPages: 3, CreditPoints: 3}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	// Abort after ledger mutations, while writing the order: all must roll back.
	if _, err := s.pay.store.db.Exec(`CREATE TRIGGER fail_order BEFORE UPDATE ON orders BEGIN SELECT RAISE(ABORT,'injected order failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.pay.store.update(order.ID, func(o *PayOrder) error { o.State = "closed"; o.PaymentClosed = true; return nil })
	current, _ := s.pay.store.get(order.ID)
	if err == nil || current.State != "pending" || current.CreditReturned || s.pay.referrals.credit("owner") != 2 {
		t.Fatal("transaction not rolled back", err, current)
	}
	if _, err := s.pay.store.db.Exec("DROP TRIGGER fail_order"); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.update(order.ID, func(o *PayOrder) error { o.State = "closed"; o.PaymentClosed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	storeDir := s.pay.store.dir
	s.pay.store.db.Close()
	s.pay.store, err = openOrderStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.pay.store.db.Close() })
	s.pay.referrals = &ReferralStore{db: s.pay.store.db}
	s.settleReferrals(current)
	s.settleReferrals(current)
	current, _ = s.pay.store.get(order.ID)
	if !current.CreditReturned || s.pay.referrals.credit("owner") != 5 {
		t.Fatal("restart replay lost or duplicated credit", current)
	}

	if err := s.grantOrder(order.ID, queriedOrder{ID: order.ID, EnvType: 1, Status: 2, WxID: "closed-wx"}); err == nil {
		t.Fatal("terminal closed order revived after credit return")
	}
}

func TestReferralConfirmedClosureAndRefundReturnOnce(t *testing.T) {
	for _, status := range []int{6, 5, 8} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := testPayServer(t)
			order := PayOrder{ID: "terminal", OpenID: "owner", State: "pending", CreditPages: 3, CreditPoints: 3}
			if err := s.pay.store.add(order); err != nil {
				t.Fatal(err)
			}
			if err := s.grantOrder(order.ID, queriedOrder{ID: order.ID, EnvType: 1, Status: status, WxID: "terminal-wx"}); err != nil {
				t.Fatal(err)
			}
			current, _ := s.pay.store.get(order.ID)
			s.settleReferrals(current)
			s.settleReferrals(current)
			if got := s.pay.referrals.credit("owner"); got != 3 {
				t.Fatal(got)
			}
		})
	}
}

func TestPointsLoginDailyCapAndChinaDay(t *testing.T) {
	s := testPayServer(t)
	now := time.Date(2026, 1, 1, 15, 59, 0, 0, time.UTC)
	inviter, err := s.pay.referrals.view("host")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pay.referrals.login("host", inviter.Code, now); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.pay.referrals.login("friend", inviter.Code, now); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s.pay.referrals.credit("host") != 100 || s.pay.referrals.credit("friend") != 100 {
		t.Fatal("duplicate first login")
	}
	for i := 0; i < 10; i++ {
		if err := s.pay.referrals.login(fmt.Sprint("friend", i), inviter.Code, now); err != nil {
			t.Fatal(err)
		}
	}
	if s.pay.referrals.credit("host") != 500 {
		t.Fatal("shared daily cap")
	}
	if n, err := s.pay.referrals.claim("host", now); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := s.pay.referrals.claim("friend", now); n != 100 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := s.pay.referrals.claim("friend", now); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	tomorrow := now.Add(2 * time.Minute)
	if n, err := s.pay.referrals.claim("host", tomorrow); n != 100 || err != nil {
		t.Fatal(n, err)
	}
	if err := s.pay.referrals.login("friend0", inviter.Code, tomorrow); err != nil {
		t.Fatal(err)
	}
	if s.pay.referrals.credit("host") != 600 {
		t.Fatal("capped event replayed tomorrow")
	}
	if s.pay.referrals.credit("friend9") != 100 {
		t.Fatal("host cap incorrectly capped friend")
	}
	wallet, err := s.pay.referrals.wallet("host", tomorrow)
	if err != nil || wallet["daily_earned"] != 100 || wallet["daily_claimed"] != true {
		t.Fatal(wallet, err)
	}
}

func TestPointsLoginRollbackAndKnownUsers(t *testing.T) {
	s := testPayServer(t)
	now := time.Now()
	host, _ := s.pay.referrals.view("host")
	s.pay.store.db.Exec(`CREATE TRIGGER fail_earn BEFORE UPDATE ON referral_users BEGIN SELECT RAISE(ABORT,'injected'); END`)
	if err := s.pay.referrals.login("friend", host.Code, now); err == nil {
		t.Fatal("partial login accepted")
	}
	s.pay.store.db.Exec("DROP TRIGGER fail_earn")
	if err := s.pay.referrals.login("friend", host.Code, now); err != nil {
		t.Fatal(err)
	}
	if s.pay.referrals.credit("host") != 100 {
		t.Fatal("failed login consumed invitation")
	}
	if err := s.pay.referrals.login("known", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.referrals.login("known", host.Code, now); err != nil {
		t.Fatal(err)
	}
	if s.pay.referrals.credit("known") != 0 {
		t.Fatal("existing user invited")
	}
}

func TestPointsWholePagesAndFailureReturn(t *testing.T) {
	cases := []struct {
		engine string
		// balance covers the whole billed document for points engines, except DeepSeek's
		// partial case below.
		balance       int
		covered, left int
	}{
		{"Bing", 29, 2, 9},
		{"DeepSeek-V4.1-Flash", 25, 2, 5},
		{"Google", 29, 0, 29},
	}
	for _, c := range cases {
		t.Run(c.engine, func(t *testing.T) {
			s := testPayServer(t)
			s.pay.cfg.Enabled = false
			if err := s.pay.referrals.restore("owner", c.balance); err != nil {
				t.Fatal(err)
			}
			o := PayOrder{ID: "points", OpenID: "owner", Engine: c.engine, Pages: 2}
			err := s.pay.store.addPurchase(&o, 2, func(o *PayOrder, n int) error { return s.pricePurchase(o, n, "ios", "") })
			per := enginePointsPerPage[c.engine]
			if per == 0 {
				if !errors.Is(err, errCashDisabled) || s.pay.referrals.credit("owner") != c.balance {
					t.Fatal(err)
				}
				return
			}
			if err != nil || o.CreditPoints != c.covered*per || o.CreditPages != c.covered || o.Total != 0 || o.State != "paid" {
				t.Fatal(o, err)
			}
			if s.pay.referrals.credit("owner") != c.left {
				t.Fatal("unexpected points spent")
			}
			if err := s.reconcileOrder(context.Background(), o.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.pay.store.update(o.ID, func(o *PayOrder) error { o.Delivery = "failed"; return nil }); err != nil {
				t.Fatal(err)
			}
			s.settleReferrals(o)
			if s.pay.referrals.credit("owner") != c.balance {
				t.Fatal("failed delivery return")
			}
			if _, err := s.startPaidOrder(context.Background(), o.ID); err == nil {
				t.Fatal("returned order restarted")
			}
		})
	}

	// DeepSeek costs 10 points per page: a balance below one page is never spent.
	s := testPayServer(t)
	s.pay.cfg.Enabled = false
	if err := s.pay.referrals.restore("owner", 9); err != nil {
		t.Fatal(err)
	}
	o := PayOrder{ID: "partial", OpenID: "owner", Engine: "DeepSeek-V4.1-Flash", Pages: 2}
	if err := s.pay.store.addPurchase(&o, 2, func(o *PayOrder, n int) error { return s.pricePurchase(o, n, "ios", "") }); !errors.Is(err, errCashDisabled) ||
		o.CreditPoints != 0 || s.pay.referrals.credit("owner") != 9 {
		t.Fatal(o, err)
	}
}

func TestBothPurchaseRoutesPointsWithoutPaymentSession(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			s := testPayServer(t)
			s.pay.cfg.Enabled = false
			s.pay.cfg.TrialEnabled = false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/pdf/inspect" {
					writeJSON(w, 200, map[string]int{"page_count": 2})
					return
				}
				if r.URL.Path == "/v1/engine/check" {
					writeJSON(w, 200, map[string]bool{"ready": true})
					return
				}
				if r.URL.Path == "/v1/translate" {
					writeJSON(w, 200, map[string]string{"id": "point-task"})
					return
				}
				t.Error(r.URL.Path)
			}))
			defer upstream.Close()
			s.pdf2zh = NewPdf2zhClient(upstream.URL)
			if err := s.pay.referrals.restore("owner", 20); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if prepared {
				path := t.TempDir() + "/source.pdf"
				content := []byte("%PDF-test")
				os.WriteFile(path, content, 0600)
				hash := sha256.Sum256(content)
				s.prepared.files = map[string]preparedPDF{"f": {Owner: "owner", Path: path, Hash: hex.EncodeToString(hash[:]), Name: "a.pdf", Pages: 2, Expires: time.Now().Add(time.Hour)}}
				s.handlePreparedOrder(w, ownRequest("POST", "/pay/order/prepared", "owner", bytes.NewBufferString(`{"file_token":"f","engine":"Bing","lang_in":"en","lang_out":"zh"}`)))
			} else {
				var body bytes.Buffer
				mw := multipart.NewWriter(&body)
				file, _ := mw.CreateFormFile("file", "a.pdf")
				file.Write([]byte("%PDF-test"))
				mw.WriteField("data", `{"engine":"Bing","lang_in":"en","lang_out":"zh"}`)
				mw.Close()
				req := ownRequest("POST", "/pay/order", "owner", &body)
				req.Header.Set("Content-Type", mw.FormDataContentType())
				s.handlePayOrder(w, req)
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var out struct {
				Order   map[string]any
				PayData any
			}
			json.Unmarshal(w.Body.Bytes(), &out)
			if out.PayData != nil || out.Order["points_spent"] != float64(20) || out.Order["state"] != "paid" {
				t.Fatal(out)
			}
			id := out.Order["id"].(string)
			if _, err := s.startPaidOrder(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			q := httptest.NewRecorder()
			s.handlePayQuery(q, ownRequest("POST", "/pay/query", "owner", bytes.NewBufferString(`{"order_id":"`+id+`"}`)))
			if q.Code != 200 {
				t.Fatal(q.Code, q.Body.String())
			}
		})
	}
}
