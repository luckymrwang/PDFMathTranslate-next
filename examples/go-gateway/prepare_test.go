package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareEvictsCompletedCacheButProtectsUploadsAndOrderFiles(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]int{"page_count": 1})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	upload := func() *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("file", "sample.pdf")
		part.Write([]byte("%PDF-test"))
		mw.Close()
		r := ownRequest("POST", "/pay/prepare", "owner", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		s.handlePreparePDF(w, r)
		return w
	}
	first := upload()
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	var result struct {
		Token string `json:"file_token"`
	}
	json.Unmarshal(first.Body.Bytes(), &result)
	oldPath := s.prepared.files[result.Token].Path
	orderPath := filepath.Join(s.pay.store.dir, "uploads", ".order-test.pdf")
	if err := os.Link(oldPath, orderPath); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		w := upload()
		if w.Code != 200 {
			t.Fatal("repeat preparation blocked", w.Code, w.Body.String())
		}
	}
	if len(s.prepared.files) != 5 {
		t.Fatal("cache no longer bounded")
	}
	if _, ok := s.prepared.files[result.Token]; ok {
		t.Fatal("oldest credential not evicted")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old cache file not removed", err)
	}
	if raw, err := os.ReadFile(orderPath); err != nil || string(raw) != "%PDF-test" {
		t.Fatal("order PDF lost", err)
	}
	// All slots are truly in-flight: no upload may be evicted to bypass the limit.
	s.prepared.Lock()
	for id, file := range s.prepared.files {
		os.Remove(file.Path)
		delete(s.prepared.files, id)
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		s.prepared.files[id] = preparedPDF{Owner: "owner"}
	}
	s.prepared.Unlock()
	if w := upload(); w.Code != 429 {
		t.Fatal("active upload cap bypassed", w.Code)
	}
}

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

// An image-only scan is still accepted: the client only gets a hint steering the
// user to OCR-capable engines, and the order remembers it for manual follow-up.
func TestPrepareFlagsScannedPDFWithoutBlocking(t *testing.T) {
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/pdf/inspect" {
			writeJSON(w, 200, map[string]any{"page_count": 4, "scanned": true})
			return
		}
		writeJSON(w, 200, map[string]bool{"ready": true})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "scan.pdf")
	part.Write([]byte("%PDF-test"))
	mw.Close()
	r := ownRequest("POST", "/pay/prepare", "owner", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	s.handlePreparePDF(w, r)
	if w.Code != 200 {
		t.Fatal("scanned PDF was blocked", w.Code, w.Body.String())
	}
	var prepared struct {
		Token       string   `json:"file_token"`
		Pages       int      `json:"page_count"`
		Scanned     bool     `json:"scanned"`
		Hint        string   `json:"scan_hint"`
		Recommended []string `json:"recommended_engines"`
		Percent     int      `json:"scan_discount_percent"`
		CapFen      int      `json:"scan_discount_cap_fen"`
	}
	json.Unmarshal(w.Body.Bytes(), &prepared)
	if !prepared.Scanned || prepared.Hint != msgScanHint || prepared.Pages != 4 ||
		prepared.Percent != scanDiscountPercent || prepared.CapFen != scanDiscountCapFen ||
		strings.Join(prepared.Recommended, ",") != "GPT-5.6,GPT-6" {
		t.Fatal("missing scan hint", w.Body.String())
	}
	payload, _ := json.Marshal(map[string]any{"file_token": prepared.Token,
		"engine": "Bing", "lang_in": "en", "lang_out": "zh"})
	req := ownRequest("POST", "/pay/order/prepared", "owner", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer token")
	out := httptest.NewRecorder()
	s.handlePreparedOrder(out, req)
	if out.Code != 200 {
		t.Fatal("scanned PDF order was refused", out.Code, out.Body.String())
	}
	orders := s.pay.store.all()
	if len(orders) != 1 || !orders[0].Scanned {
		t.Fatal("order did not keep the scan flag")
	}
	failed := orders[0]
	failed.Delivery = "failed"
	if got := paidFailureMessage(failed); got != msgScanFailed {
		t.Fatal("scanned failure should ask for manual handling, not a refund:", got)
	}
}

func TestScannedOrdersGetTheScanDiscount(t *testing.T) {
	for _, c := range []struct{ pages, unit, want int }{
		{1, 30, 0}, // a one-page scan is never free
		{3, 30, 1}, // 20% of 3 pages, rounded up to a page
		{7, 30, 2},
		{10, 30, 2},
		{500, 30, 66}, // the ¥20 cap at ¥0.30/page
		{100, 40, 20}, // GPT-6 prices
		{10, 0, 2},    // unpriced engine: no cap to apply
	} {
		if got := scanDiscountPages(c.pages, c.unit); got != c.want {
			t.Fatalf("scanDiscountPages(%d, %d) = %d, want %d", c.pages, c.unit, got, c.want)
		}
	}

	s := testPayServer(t)
	store := s.pay.store
	buy := func(owner, engine string, pages int, scanned bool) PayOrder {
		t.Helper()
		id, _ := newTradeNo()
		order := PayOrder{ID: id, OpenID: owner, Pages: pages, Engine: engine, State: "pending",
			Scanned: scanned, CreatedAt: time.Now().Unix()}
		free := s.scanDiscountFor(engine, pages, scanned)
		if err := store.addPurchase(&order, pages, free, func(o *PayOrder, remaining int) error {
			o.Quantity, o.Total = remaining, remaining*30
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return order
	}
	setState := func(id, state string) {
		t.Helper()
		if err := store.update(id, func(o *PayOrder) error { o.State = state; return nil }); err != nil {
			t.Fatal(err)
		}
	}

	first := buy("owner", "GPT-5.6", 10, true)
	if first.ScanFreePages != 2 || first.Quantity != 8 || first.Total != 240 {
		t.Fatal("a 10-page scan should bill 8 pages", first.ScanFreePages, first.Quantity, first.Total)
	}
	setState(first.ID, "paid")
	if again := buy("owner", "GPT-5.6", 10, true); again.ScanFreePages != 2 {
		t.Fatal("every scanned order is discounted, not only the first", again.ScanFreePages)
	}
	for name, order := range map[string]PayOrder{
		"text PDF":      buy("text", "GPT-5.6", 10, false),
		"one-page scan": buy("tiny", "GPT-5.6", 1, true),
	} {
		if order.ScanFreePages != 0 || order.Quantity != order.Pages {
			t.Fatal(name+" must not be discounted", order.ScanFreePages, order.Quantity)
		}
	}
}

func TestPointsOrderFailureTellsUserWhatHappened(t *testing.T) {
	scan := PayOrder{Engine: "Bing", CreditPoints: 70, Scanned: true, Delivery: "failed"}
	if got := paidFailureMessage(scan); got != msgScanPointsFailed {
		t.Fatal("a scanned points order should say why and how to reorder:", got)
	}
	text := PayOrder{Engine: "Bing", CreditPoints: 70, Delivery: "failed"}
	if got := paidFailureMessage(text); got != "翻译失败，本单积分已退回" {
		t.Fatal(got)
	}
	cash := PayOrder{Engine: "Bing", Total: 70, Scanned: true, Delivery: "failed"}
	if got := paidFailureMessage(cash); got != msgScanFailed {
		t.Fatal("a scanned cash order is finished by hand:", got)
	}
	if !strings.Contains(scanReorderNote(scan), "GPT-5.6") || scanReorderNote(text) != "" {
		t.Fatal("operator note should only suggest reordering scans")
	}
}
