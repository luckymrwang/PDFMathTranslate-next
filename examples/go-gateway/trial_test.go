package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestFreeTrialIsOncePerUserAndTranslatesFirstPage(t *testing.T) {
	s := testPayServer(t)
	var forwarded map[string]any
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/engine/check":
			io.Copy(io.Discard, r.Body)
			writeJSON(w, 200, map[string]bool{"ready": true})
		case "/v1/translate":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			json.Unmarshal([]byte(r.FormValue("data")), &forwarded)
			writeJSON(w, 200, map[string]string{"id": "task-1"})
		default:
			t.Error("unexpected upstream", r.URL.Path)
		}
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	s.pay.sessions.set("token", "session")

	trial := func(owner string) *httptest.ResponseRecorder {
		s.prepared.Lock()
		s.prepared.files = map[string]preparedPDF{}
		s.prepared.Unlock()
		path := t.TempDir() + "/a.pdf"
		content := []byte("%PDF-test")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		s.prepared.Lock()
		s.prepared.files["f"] = preparedPDF{Owner: owner, Path: path, Name: "a.pdf", Hash: hex.EncodeToString(sum[:]),
			Pages: 30, Expires: time.Now().Add(time.Hour)}
		s.prepared.Unlock()
		// Paid engine and price are ignored for a trial.
		payload, _ := json.Marshal(map[string]any{"file_token": "f", "trial": true,
			"engine": "GPT-6", "lang_in": "en", "lang_out": "zh"})
		req := ownRequest("POST", "/pay/order/prepared", owner, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer token")
		out := httptest.NewRecorder()
		s.handlePreparedOrder(out, req)
		return out
	}

	got := trial("owner")
	if got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	var created struct {
		Order   map[string]any    `json:"order"`
		PayData map[string]string `json:"payData"`
	}
	json.Unmarshal(got.Body.Bytes(), &created)
	if created.Order["state"] != "paid" || created.Order["trial"] != true ||
		created.Order["engine"] != trialEngine || created.Order["total_fen"].(float64) != 0 || created.PayData != nil {
		t.Fatal("unexpected trial order", got.Body.String())
	}
	if again := trial("owner"); again.Code != 409 {
		t.Fatal("second trial allowed", again.Code)
	}
	if other := trial("someone-else"); other.Code != 200 {
		t.Fatal("trial should be per user", other.Code)
	}

	id := created.Order["id"].(string)
	if _, err := s.startPaidOrder(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if forwarded["pages"] != "1" {
		t.Fatal("trial must translate only page 1:", forwarded)
	}
	if err := s.reconcileOrder(context.Background(), id); err != nil {
		t.Fatal("trial must not query WeChat:", err)
	}
}
