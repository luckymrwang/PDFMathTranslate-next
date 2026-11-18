package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Chart and diagram text stays as it is unless the order asked to translate it.
func TestPaidOrderSubmitsFigureChoice(t *testing.T) {
	var seen []map[string]any
	upstream := captureUpstream(t, &seen)
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "source.pdf")
	content := []byte("%PDF-1.7\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	srv := testPayServer(t)
	srv.pdf2zh = NewPdf2zhClient(upstream.URL)
	for _, figures := range []bool{false, true} {
		order := PayOrder{ID: "order-1", Engine: "Bing", LangIn: "en", LangOut: "zh", Name: "source.pdf",
			FilePath: path, FileHash: hex.EncodeToString(sum[:]), TranslateFigures: figures}
		_, _ = srv.submitPaidOrder(context.Background(), order)
		if view := orderView(order); view["translate_figures"] != figures {
			t.Errorf("order view translate_figures = %v, want %v", view["translate_figures"], figures)
		}
	}
	if len(seen) != 2 || seen[0]["skip_image_translation"] != true || seen[1]["skip_image_translation"] != false {
		t.Fatalf("wrong skip_image_translation sent: %v", seen)
	}
}

func TestOrdersStoreFigureChoice(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pdf/inspect" {
			writeJSON(w, 200, map[string]int{"page_count": 2})
			return
		}
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	pdf := func(mw *multipart.Writer) {
		part, _ := mw.CreateFormFile("file", "sample.pdf")
		part.Write([]byte("%PDF-test"))
	}

	// Prepared file path.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	pdf(mw)
	mw.WriteField("data", `{"file_name":"a.pdf"}`)
	mw.Close()
	r := ownRequest("POST", "/pay/prepare", "owner", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handlePreparePDF(w, r)
	var prepared struct {
		Token string `json:"file_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &prepared)
	if w.Code != 200 || prepared.Token == "" {
		t.Fatal("prepare failed", w.Code, w.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"file_token": prepared.Token,
		"engine": "Bing", "lang_in": "en", "lang_out": "zh", "translate_figures": true})
	req := ownRequest("POST", "/pay/order/prepared", "owner", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer token")
	out := httptest.NewRecorder()
	s.handlePreparedOrder(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	var created struct {
		Order map[string]any `json:"order"`
	}
	json.Unmarshal(out.Body.Bytes(), &created)
	if created.Order["translate_figures"] != true {
		t.Errorf("prepared order response: %v", created.Order)
	}

	// Direct upload path (older clients); the field is left out, so it stays off.
	body.Reset()
	mw = multipart.NewWriter(&body)
	pdf(mw)
	mw.WriteField("data", `{"engine":"Bing","lang_in":"en","lang_out":"zh"}`)
	mw.Close()
	req = ownRequest("POST", "/pay/order", "owner", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer token")
	out = httptest.NewRecorder()
	s.handlePayOrder(out, req)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}

	figures := map[bool]int{}
	for _, o := range s.pay.store.all() {
		figures[o.TranslateFigures]++
	}
	if figures[true] != 1 || figures[false] != 1 {
		t.Fatalf("stored figure choices = %v, want one on and one off", figures)
	}
}
