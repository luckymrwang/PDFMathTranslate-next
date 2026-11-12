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

func TestOutputParams(t *testing.T) {
	cases := []struct {
		output         string
		noMono, noDual bool
	}{
		{"", true, false}, // older clients and orders: dual only, as before
		{"dual", true, false},
		{"mono", false, true},
		{"both", false, false},
	}
	for _, c := range cases {
		params := map[string]any{}
		outputParams(params, c.output)
		if params["no_mono"] != c.noMono || params["no_dual"] != c.noDual {
			t.Errorf("output %q: got no_mono=%v no_dual=%v", c.output, params["no_mono"], params["no_dual"])
		}
	}
	if _, ok := parseOutput("pdf"); ok {
		t.Error("unknown output mode accepted")
	}
	if got, ok := parseOutput(""); !ok || got != "dual" {
		t.Errorf("empty output should mean dual, got %q", got)
	}
}

// captureUpstream records the data form field of each /v1/translate request.
func captureUpstream(t *testing.T, seen *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("data")), &data); err != nil {
			t.Errorf("invalid forwarded data: %v", err)
		}
		*seen = append(*seen, data)
		_, _ = io.WriteString(w, `{"id":"job-1"}`)
	}))
}

func TestTranslateSubmitForwardsOutput(t *testing.T) {
	var seen []map[string]any
	upstream := captureUpstream(t, &seen)
	defer upstream.Close()
	submit := func(extra string) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("file", "sample.pdf")
		_, _ = part.Write([]byte("%PDF-1.7\n"))
		_ = mw.WriteField("data", `{"lang_in":"en","lang_out":"zh","engine":"Bing"`+extra+`}`)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/translate", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyOpenID, "openid-1"))
		w := httptest.NewRecorder()
		srv := &Server{pdf2zh: NewPdf2zhClient(upstream.URL), tasks: newTaskRegistry()}
		srv.handleTranslateSubmit(w, req)
		return w.Code
	}
	if code := submit(`,"output":"both"`); code != http.StatusOK {
		t.Fatalf("submit failed: %d", code)
	}
	if code := submit(``); code != http.StatusOK {
		t.Fatalf("submit failed: %d", code)
	}
	if code := submit(`,"output":"html"`); code != http.StatusBadRequest {
		t.Fatalf("invalid output accepted: %d", code)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 upstream submissions, got %d", len(seen))
	}
	if seen[0]["no_mono"] != false || seen[0]["no_dual"] != false {
		t.Errorf("both: wrong flags %v", seen[0])
	}
	if seen[1]["no_mono"] != true || seen[1]["no_dual"] != false {
		t.Errorf("default: wrong flags %v", seen[1])
	}
}

func TestPaidOrderSubmitsOutput(t *testing.T) {
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
	order := PayOrder{ID: "order-1", Engine: "Bing", LangIn: "en", LangOut: "zh", Name: "source.pdf",
		FilePath: path, FileHash: hex.EncodeToString(sum[:]), Output: "mono"}
	// The order is not in the store, so recording the task fails after the upload;
	// only the parameters sent upstream matter here.
	_, _ = srv.submitPaidOrder(context.Background(), order)
	if len(seen) != 1 || seen[0]["no_mono"] != false || seen[0]["no_dual"] != true {
		t.Fatalf("mono order sent wrong flags: %v", seen)
	}
	if view := orderView(order); view["output"] != "mono" {
		t.Errorf("order view output = %v", view["output"])
	}
	if view := orderView(PayOrder{}); view["output"] != "dual" {
		t.Errorf("legacy order view output = %v", view["output"])
	}
}

func TestPreparedOrderStoresOutput(t *testing.T) {
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
	prepare := func() string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("file", "sample.pdf")
		part.Write([]byte("%PDF-test"))
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
		return prepared.Token
	}
	order := func(token, output string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{"file_token": token,
			"engine": "Bing", "lang_in": "en", "lang_out": "zh", "output": output})
		req := ownRequest("POST", "/pay/order/prepared", "owner", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, req)
		return out
	}
	token := prepare()
	if got := order(token, "pdf"); got.Code != 400 {
		t.Fatal("invalid output accepted", got.Code)
	}
	if got := order(token, "both"); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	orders := s.pay.store.all()
	if len(orders) != 1 || orders[0].Output != "both" {
		t.Fatalf("output not stored: %+v", orders)
	}
}
