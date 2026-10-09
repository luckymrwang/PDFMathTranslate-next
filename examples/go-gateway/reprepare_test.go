package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReprepareUsesVerifiedServerFileAndNewOrders(t *testing.T) {
	s := testPayServer(t)
	raw := []byte("%PDF-preserved")
	path := filepath.Join(s.pay.store.dir, "uploads", ".order-original.pdf")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	old := PayOrder{ID: "Toriginal", OpenID: "owner", State: "closed", Name: "Original.pdf", Quantity: 16, FilePath: path, FileHash: hex.EncodeToString(hash[:])}
	if err := s.pay.store.add(old); err != nil {
		t.Fatal(err)
	}
	prepare := func(owner string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleRepreparePDF(w, ownRequest("POST", "/pay/prepare/order", owner, bytes.NewBufferString(`{"order_id":"Toriginal","page_count":1}`)))
		return w
	}
	if w := prepare("stranger"); w.Code != 404 {
		t.Fatal("ownership bypass", w.Code)
	}
	w := prepare("owner")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Token string `json:"file_token"`
		Pages int    `json:"page_count"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Pages != 16 || result.Token == "" {
		t.Fatal("trusted pages lost")
	}
	if len(s.pay.store.all()) != 1 {
		t.Fatal("preparing file created an order")
	}
	// Only model checking may call Python; never inspect/upload the PDF again.
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/engine/check" {
			t.Error("unexpected file upload", r.URL.Path)
		}
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	for i := 0; i < 2; i++ {
		payload, _ := json.Marshal(map[string]any{"file_token": result.Token, "engine": "Bing", "lang_in": "en", "lang_out": "zh"})
		r := ownRequest("POST", "/pay/order/prepared", "owner", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, r)
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	if len(s.pay.store.all()) != 3 {
		t.Fatal("new unique orders missing")
	}
	s.prepared.Lock()
	cache := s.prepared.files[result.Token]
	os.Remove(cache.Path)
	delete(s.prepared.files, result.Token)
	s.prepared.Unlock()
	for _, order := range s.pay.store.all() {
		if _, err := os.Stat(order.FilePath); err != nil {
			t.Fatal("order file lost", err)
		}
		if order.ID != old.ID && (order.Quantity != 16 || order.Total != 160 || order.Name != "Original.pdf") {
			t.Fatal("new order metadata incorrect")
		}
	}
	// Terminal status and integrity are mandatory, even with matching names.
	s.pay.store.update(old.ID, func(order *PayOrder) error { order.State = "pending"; return nil })
	if w := prepare("owner"); w.Code != 409 {
		t.Fatal("pending order reused", w.Code)
	}
	s.pay.store.update(old.ID, func(order *PayOrder) error { order.State = "refunded"; return nil })
	if w := prepare("owner"); w.Code != 200 {
		t.Fatal("refunded file cannot be reused", w.Code)
	}
	os.WriteFile(path, []byte("%PDF-changed"), 0600)
	if w := prepare("owner"); w.Code != 410 {
		t.Fatal("changed file accepted", w.Code)
	}
	os.Remove(path)
	if w := prepare("owner"); w.Code != 410 {
		t.Fatal("missing file accepted", w.Code)
	}
}
