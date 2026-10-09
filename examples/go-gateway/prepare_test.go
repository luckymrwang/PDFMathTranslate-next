package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestPreparedFileOrderReuseAndOwnership(t *testing.T) {
	s := testPayServer(t)
	inspections := 0
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pdf/inspect" {
			inspections++
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("data") != "" {
				t.Error("file inspection must not check model")
			}
			writeJSON(w, 200, map[string]int{"page_count": 16})
			return
		}
		if r.URL.Path != "/v1/engine/check" {
			t.Error("unexpected upstream", r.URL.Path)
		}
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "sample.pdf")
	part.Write([]byte("%PDF-test"))
	mw.WriteField("data", `{"file_name":"原始文档.pdf"}`)
	mw.Close()
	r := ownRequest("POST", "/pay/prepare", "owner", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handlePreparePDF(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var prepared struct {
		Token string `json:"file_token"`
		Pages int    `json:"page_count"`
	}
	json.Unmarshal(w.Body.Bytes(), &prepared)
	if prepared.Pages != 16 || prepared.Token == "" {
		t.Fatal("missing file credential")
	}
	submit := func(owner string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{"file_token": prepared.Token,
			"engine": "Bing", "lang_in": "en", "lang_out": "zh", "quantity": 1, "goodsPrice": 1})
		req := ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, req)
		return out
	}
	if got := submit("stranger"); got.Code != 410 {
		t.Fatal("cross-user reuse allowed", got.Code)
	}
	for i := 0; i < 2; i++ {
		got := submit("owner")
		if got.Code != 200 {
			t.Fatal(got.Code, got.Body.String())
		}
	}
	if inspections != 1 {
		t.Fatal("PDF was uploaded again")
	}
	orders := s.pay.store.all()
	for _, order := range orders {
		if order.Name != "原始文档.pdf" {
			t.Fatal("original PDF name lost", order.Name)
		}
		if order.Quantity != 16 || order.Total != 160 {
			t.Fatal("client changed pricing")
		}
	}
	s.prepared.Lock()
	f := s.prepared.files[prepared.Token]
	os.Remove(f.Path)
	f.Expires = time.Now().Add(-time.Second)
	s.prepared.files[prepared.Token] = f
	s.prepared.Unlock()
	if got := submit("owner"); got.Code != 410 {
		t.Fatal("expired credential accepted")
	}
	for _, order := range orders {
		if _, err := os.Stat(order.FilePath); err != nil {
			t.Fatal("order file lost after expiry", err)
		}
	}
}
