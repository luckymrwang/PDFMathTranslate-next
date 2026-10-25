package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestReferralRewardsDeliveredTrialsAndPaysForStandardPages(t *testing.T) {
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
	deliver := func(id string) {
		s.pay.store.update(id, func(o *PayOrder) error { o.State, o.Delivery = "submitted", "archived"; return nil })
		current, _ := s.pay.store.get(id)
		s.settleReferrals(current)
		s.settleReferrals(current)
	}

	// The inviter's own code never binds, and neither does a code on a returning user's trial.
	own := order("inviter", map[string]any{"trial": true, "engine": "Bing", "ref": view.Code})["order"].(map[string]any)
	deliver(own["id"].(string))
	if s.pay.referrals.credit("inviter") != 0 {
		t.Fatal("self-invite rewarded")
	}
	const friends = 12
	for i := 0; i < friends; i++ {
		trial := order(fmt.Sprintf("friend-%d", i), map[string]any{"trial": true, "engine": "Bing", "ref": view.Code})["order"].(map[string]any)
		if i == 0 && s.pay.referrals.credit("inviter") != 0 {
			t.Fatal("rewarded before the trial was delivered")
		}
		deliver(trial["id"].(string))
	}
	if got := s.pay.referrals.credit("inviter"); got != friends*referralRewardPages {
		t.Fatal("reward must be per delivered trial, once each, without a cap", got)
	}

	// Credit only covers standard translation; a fully covered order needs no payment.
	if _, err := s.pay.referrals.db.Exec("UPDATE referral_users SET credit=15 WHERE openid='inviter'"); err != nil {
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
	if got := s.pay.referrals.credit("inviter"); got != 3 {
		t.Fatal("closed order credit not returned once", got)
	}
	// Reopening the store keeps codes and balances.
	reopened, err := openReferralStore(s.pay.store.dir)
	if err == nil {
		defer reopened.db.Close()
	}
	if err != nil || reopened.credit("inviter") != 3 || reopened.inviter(view.Code, "someone") != "inviter" {
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
	order := PayOrder{ID: "late-credit", OpenID: "owner", State: "closed", CreditPages: 3, Total: 90}
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
	order := PayOrder{ID: "return-credit", OpenID: "owner", State: "pending", CreditPages: 3}
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
			order := PayOrder{ID: "terminal", OpenID: "owner", State: "pending", CreditPages: 3}
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
