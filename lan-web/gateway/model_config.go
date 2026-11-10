package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const lanEngineName = "OpenAI"

type lanModelConfig struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
}
type lanModelStore struct {
	mu     sync.RWMutex
	path   string
	config lanModelConfig
}

func validModelConfig(c lanModelConfig) bool {
	u, err := url.Parse(c.BaseURL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.ContainsAny(c.BaseURL, "\r\n\t ") && len(c.BaseURL) <= 2048 && strings.TrimSpace(c.Model) == c.Model && c.Model != "" && len(c.Model) <= 256 && len(c.APIKey) <= 8192 && !strings.ContainsAny(c.APIKey, "\r\n")
}
func openLanModelStore(dir string) (*lanModelStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &lanModelStore{path: filepath.Join(dir, "model-config.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &s.config); err != nil || !validModelConfig(s.config) || s.config.APIKey == "" {
		return nil, errors.New("invalid saved model configuration")
	}
	return s, nil
}
func (s *lanModelStore) get() lanModelConfig { s.mu.RLock(); defer s.mu.RUnlock(); return s.config }
func (s *lanModelStore) put(c lanModelConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.APIKey == "" {
		c.APIKey = s.config.APIKey
	}
	if !validModelConfig(c) || c.APIKey == "" {
		return errors.New("invalid model configuration")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".model-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.config = c
	return nil
}
func (s *Server) handleLanModelConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var c lanModelConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&c); err != nil {
			writeError(w, 400, "invalid request body")
			return
		}
		// Validate input before writing; the store checks the retained key under lock.
		if !validModelConfig(c) || (c.APIKey == "" && s.lanSettings.get().APIKey == "") {
			writeError(w, 400, "invalid model configuration")
			return
		}
		if err := s.lanSettings.put(c); err != nil {
			writeError(w, 500, "failed to save model configuration")
			return
		}
	default:
		writeError(w, 405, "method not allowed")
		return
	}
	c := s.lanSettings.get()
	writeJSON(w, 200, map[string]any{"base_url": c.BaseURL, "model": c.Model, "has_api_key": c.APIKey != ""})
}
func (s *Server) lanEngine(name string) (map[string]string, bool) {
	c := s.lanSettings.get()
	if name != lanEngineName || c.APIKey == "" {
		return nil, false
	}
	return map[string]string{"translate_engine_type": "OpenAI", "openai_base_url": c.BaseURL, "openai_model": c.Model, "openai_api_key": c.APIKey}, true
}
func (s *Server) handleLanEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	names := []string{}
	if _, ok := s.lanEngine(lanEngineName); ok {
		names = append(names, lanEngineName)
	}
	writeJSON(w, 200, map[string]any{"engines": names})
}
