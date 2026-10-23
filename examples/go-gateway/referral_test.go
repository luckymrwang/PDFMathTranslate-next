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
	for i := 0; i < referralCap+1; i++ {
		trial := order(fmt.Sprintf("friend-%d", i), map[string]any{"trial": true, "engine": "Bing", "ref": view.Code})["order"].(map[string]any)
		if i == 0 && s.pay.referrals.credit("inviter") != 0 {
			t.Fatal("rewarded before the trial was delivered")
		}
		deliver(trial["id"].(string))
	}
	if got := s.pay.referrals.credit("inviter"); got != referralCap*referralRewardPages {
		t.Fatal("reward must be per delivered trial, once each, capped", got)
	}

	// Credit only covers standard translation; a fully covered order needs no payment.
	s.pay.referrals.mu.Lock()
	s.pay.referrals.users["inviter"].Credit = 15
	s.pay.referrals.mu.Unlock()
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
	s.pay.store.update(partial["id"].(string), func(o *PayOrder) error { o.State = "closed"; return nil })
	for i := 0; i < 2; i++ {
		current, _ := s.pay.store.get(partial["id"].(string))
		s.settleReferrals(current)
	}
	if got := s.pay.referrals.credit("inviter"); got != 3 {
		t.Fatal("closed order credit not returned once", got)
	}
	// Reopening the store keeps codes and balances.
	reopened, err := openReferralStore(s.pay.store.dir)
	if err != nil || reopened.credit("inviter") != 3 || reopened.inviter(view.Code, "someone") != "inviter" {
		t.Fatal("referrals not persisted", err)
	}
}
