package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitResolvesEngineOnServer(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "server-only-secret")
	t.Setenv("OPENAI_MODEL", "gpt-6-luna")
	t.Setenv("OPENAI_BASE_URL", "https://example.test/v1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/translate" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("data")), &data); err != nil {
			t.Errorf("invalid forwarded data: %v", err)
		}
		settings, _ := data["translate_engine_settings"].(map[string]any)
		if settings["openai_api_key"] != "server-only-secret" || settings["openai_model"] != "gpt-6-luna" || settings["openai_base_url"] != "https://example.test/v1" {
			t.Errorf("wrong server-side settings: %v", settings)
		}
		if _, present := data["engine"]; present {
			t.Error("client-only engine field forwarded")
		}
		if data["skip_image_translation"] != true {
			t.Error("image protection must default to enabled")
		}
		_, _ = io.WriteString(w, `{"id":"job-1"}`)
	}))
	defer upstream.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "sample.pdf")
	_, _ = part.Write([]byte("%PDF-1.7\n"))
	_ = mw.WriteField("data", `{"lang_in":"en","lang_out":"zh","engine":"GPT-6","translate_engine_settings":{"translate_engine_type":"Google","openai_api_key":"client-secret"}}`)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/translate", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyOpenID, "openid-1"))
	w := httptest.NewRecorder()
	srv := &Server{pdf2zh: NewPdf2zhClient(upstream.URL), tasks: newTaskRegistry()}
	srv.handleTranslateSubmit(w, req)
	if w.Code != http.StatusOK || !srv.tasks.owns("job-1", "openid-1") {
		t.Fatalf("submit failed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestEnginesDoNotExposeSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "server-only-secret")
	w := httptest.NewRecorder()
	(&Server{}).handleEngines(w, httptest.NewRequest(http.MethodGet, "/api/engines", nil))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("GPT-6")) || bytes.Contains(w.Body.Bytes(), []byte("server-only-secret")) {
		t.Fatalf("unsafe catalog response: status=%d body=%s", w.Code, w.Body.String())
	}
}
