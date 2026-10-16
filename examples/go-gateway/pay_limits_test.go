package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOldPendingOrdersDoNotPermanentlyBlockPayment(t *testing.T) {
	s := testPayServer(t)
	for i := 0; i < 10; i++ {
		if err := s.pay.store.add(PayOrder{ID: fmt.Sprintf("old-%d", i), OpenID: "owner", State: "pending",
			CreatedAt: time.Now().Add(-time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.pay.store.recentOrderCount("owner"); got != 0 {
		t.Fatal("old orders counted", got)
	}
	for i := 0; i < 10; i++ {
		if err := s.pay.store.add(PayOrder{ID: fmt.Sprintf("new-%d", i), OpenID: "owner", State: "pending",
			CreatedAt: time.Now().Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.pay.store.recentOrderCount("owner"); got != 10 {
		t.Fatal("burst not counted", got)
	}
	if got := s.pay.store.recentOrderCount("another"); got != 0 {
		t.Fatal("owners not isolated", got)
	}
	if len(s.pay.store.all()) != 20 {
		t.Fatal("historical payment records removed")
	}
}

func TestQueryOfUnpaidOrderStaysPendingWithoutServerError(t *testing.T) {
	s := testPayServer(t)
	queries := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/stable_token" {
			writeJSON(w, 200, map[string]any{"access_token": "token", "expires_in": 7200})
			return
		}
		queries++
		writeJSON(w, 200, map[string]any{"errcode": 268490002})
	}))
	defer upstream.Close()
	s.pay.client = &XPayClient{BaseURL: upstream.URL, HTTP: upstream.Client(), AppKey: "app-key", AppSecret: "secret"}
	if err := s.pay.store.add(PayOrder{ID: "never-paid", OpenID: "owner", State: "pending",
		CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	ask := func() string {
		w := httptest.NewRecorder()
		s.handlePayQuery(w, ownRequest("POST", "/pay/query", "owner", bytes.NewBufferString(`{"order_id":"never-paid"}`)))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out.State
	}
	if state := ask(); state != "pending" {
		t.Fatal("unpaid order must stay pending, got", state)
	}
	ask()
	if queries != 1 {
		t.Fatal("repeated taps must be throttled, platform queried", queries)
	}
}

func TestQueryExposesOnlyWechatErrorCode(t *testing.T) {
	s := testPayServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"errcode": 40164, "errmsg": "sensitive upstream detail"})
	}))
	defer upstream.Close()
	s.pay.client = &XPayClient{BaseURL: upstream.URL, HTTP: upstream.Client(), AppSecret: "private-key"}
	if err := s.pay.store.add(PayOrder{ID: "test-query", OpenID: "owner", State: "pending"}); err != nil {
		t.Fatal(err)
	}
	r := ownRequest("POST", "/pay/query", "owner", bytes.NewBufferString(`{"order_id":"test-query"}`))
	w := httptest.NewRecorder()
	s.handlePayQuery(w, r)
	var out struct {
		Code int `json:"wechat_error_code"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 502 || out.Code != 40164 {
		t.Fatal(w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("private-key")) || bytes.Contains(w.Body.Bytes(), []byte("sensitive")) {
		t.Fatal("query leaked secret details")
	}
}
