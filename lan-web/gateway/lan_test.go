package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLanLoginAndOldTokens(t *testing.T) {
	s := &Server{cfg: &Config{LanMode: true, LanUsername: "wangchl01", LanPassword: "admin", JWTSecret: []byte("0123456789abcdef0123456789abcdef"), TokenTTL: time.Hour}}
	for _, body := range []string{`{"device_id":"abcdefgh","password":"admin"}`, `{"device_id":"abcdefgh","username":"wrong","password":"admin"}`} {
		w := httptest.NewRecorder()
		s.handleLanLogin(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(body)))
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.handleLanLogin(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"device_id":"abcdefgh","username":"wangchl01","password":"admin"}`)))
	var login loginResponse
	json.Unmarshal(w.Body.Bytes(), &login)
	if w.Code != 200 || login.Token == "" {
		t.Fatal(w.Body.String())
	}
	old, _ := IssueToken(s.cfg.JWTSecret, "lan-abcdefgh", time.Hour)
	h := s.authRequired(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	check := func(token string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(w, r)
		return w.Code
	}
	if check(old) != 401 || check(login.Token) != 204 {
		t.Fatal("token isolation failed")
	}
	s.cfg.LanPassword = "new"
	if check(login.Token) != 401 {
		t.Fatal("old credential token accepted")
	}
}
func TestModelConfigPersistenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	store, err := openLanModelStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lanSettings: store}
	request := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleLanModelConfig(w, httptest.NewRequest(method, "/api/model-config", strings.NewReader(body)))
		return w
	}
	for _, u := range []string{"file:///etc/passwd", "http://user:password@example.com/v1", "https://example.com/v1?secret=x", "https://example.com/#fragment"} {
		b, _ := json.Marshal(lanModelConfig{BaseURL: u, Model: "model", APIKey: "secret"})
		if w := request("PUT", string(b)); w.Code != 400 {
			t.Fatalf("%s: %d", u, w.Code)
		}
	}
	w := request("PUT", `{"base_url":"http://localhost:8000/v1","model":"m1","api_key":"secret-key"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret-key") {
		t.Fatal(w.Body.String())
	}
	w = request("PUT", `{"base_url":"http://localhost:8000/v1","model":"m2","api_key":""}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret-key") {
		t.Fatal(w.Body.String())
	}
	reloaded, err := openLanModelStore(dir)
	if err != nil || reloaded.get().APIKey != "secret-key" || reloaded.get().Model != "m2" {
		t.Fatal("persistence failed", err)
	}
	st, err := os.Stat(reloaded.path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("permissions", err)
	}
}
func TestLanTranslationUsesStoredEngine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		var data map[string]any
		json.Unmarshal([]byte(r.FormValue("data")), &data)
		settings, _ := data["translate_engine_settings"].(map[string]any)
		if settings["openai_api_key"] != "server-secret" || settings["openai_base_url"] != "http://localhost:8000/v1" || settings["openai_model"] != "model" {
			t.Errorf("unexpected settings: %v", settings)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"task1"}`)
	}))
	defer upstream.Close()
	store, _ := openLanModelStore(t.TempDir())
	if err := store.put(lanModelConfig{BaseURL: "http://localhost:8000/v1", Model: "model", APIKey: "server-secret"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &Config{LanMode: true}, lanSettings: store, pdf2zh: NewPdf2zhClient(upstream.URL), tasks: newTaskRegistry()}
	submit := func(engine string) int {
		var b bytes.Buffer
		m := multipart.NewWriter(&b)
		f, _ := m.CreateFormFile("file", "a.pdf")
		f.Write([]byte("%PDF-test"))
		m.WriteField("data", `{"engine":"`+engine+`","lang_in":"en","lang_out":"zh","translate_engine_settings":{"openai_api_key":"client"}}`)
		m.Close()
		r := httptest.NewRequest("POST", "/api/translate", &b)
		r.Header.Set("Content-Type", m.FormDataContentType())
		w := httptest.NewRecorder()
		s.handleTranslateSubmit(w, r)
		return w.Code
	}
	if got := submit("Bing"); got != 400 {
		t.Fatal(got)
	}
	if got := submit("OpenAI"); got != 200 {
		t.Fatal(got)
	}
}
