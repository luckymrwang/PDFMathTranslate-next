package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultipleModelsAndLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model-config.json"), []byte(`{"base_url":"http://localhost/v1","model":"old","api_key":"old-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openLanModelStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{lanSettings: store}
	call := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.handleLanModelConfig(w, httptest.NewRequest(method, "/api/model-config", strings.NewReader(body)))
		return w
	}
	w := call("GET", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "old-secret") || !strings.Contains(w.Body.String(), `"id":"OpenAI"`) {
		t.Fatal(w.Body.String())
	}
	w = call("PUT", `{"label":"second","base_url":"https://example.com/v1","model":"new","api_key":"new-secret"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "new-secret") {
		t.Fatal(w.Code, w.Body.String())
	}
	var created publicLanModel
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatal(err, w.Body.String())
	}
	w = call("PUT", `{"id":"`+created.ID+`","label":"second","base_url":"https://example.com/v1","model":"updated","api_key":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("PUT", `{"label":"bad","base_url":"https://example.com/v1","model":"new","api_key":""}`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	reloaded, err := openLanModelStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	server.lanSettings = reloaded
	first, ok := server.lanEngine("OpenAI")
	if !ok || first["openai_api_key"] != "old-secret" {
		t.Fatal("legacy key lost")
	}
	second, ok := server.lanEngine(created.ID)
	if !ok || second["openai_api_key"] != "new-secret" || second["openai_model"] != "updated" {
		t.Fatal("second model incorrect")
	}
	w = call("GET", "")
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("key leaked")
	}
}
