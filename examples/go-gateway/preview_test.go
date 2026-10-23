package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPreviewRendersFirstPageOnceAndExpiresWithOrder(t *testing.T) {
	s := testPayServer(t)
	var renders atomic.Int32
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pdf/render" {
			t.Error("unexpected upstream", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil || !strings.Contains(r.FormValue("data"), `"page":1`) {
			t.Error("render must ask for the first page", err)
		}
		renders.Add(1)
		w.Write([]byte("\xff\xd8jpeg"))
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	mono := t.TempDir() + "/mono.pdf"
	os.WriteFile(mono, []byte("%PDF-mono"), 0600)
	order := PayOrder{ID: "trial-preview", OpenID: "owner", State: "submitted", Trial: true, Delivery: "archived",
		TaskID: "task-1", MonoFile: mono, CreatedAt: 1}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	get := func() *httptest.ResponseRecorder {
		current, _ := s.pay.store.get(order.ID)
		w := httptest.NewRecorder()
		s.handlePaidTask(w, ownRequest("GET", "/api/translate/task-1/preview", "owner", nil), current, "task-1", "preview")
		return w
	}
	for i := 0; i < 2; i++ {
		got := get()
		body, _ := io.ReadAll(got.Body)
		if got.Code != 200 || got.Header().Get("Content-Type") != "image/jpeg" || string(body) != "\xff\xd8jpeg" {
			t.Fatal(got.Code, got.Header(), string(body))
		}
	}
	if renders.Load() != 1 {
		t.Fatal("preview must be rendered once and then served from disk", renders.Load())
	}
	if err := s.expireOrder(t.Context(), order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.previewPath(order.ID)); !os.IsNotExist(err) {
		t.Fatal("preview must be deleted with the order files", err)
	}
	if got := get(); got.Code != http.StatusGone {
		t.Fatal("expired preview still served", got.Code)
	}
}
