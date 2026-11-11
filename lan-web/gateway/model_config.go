package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const lanEngineName = "OpenAI"

type lanModelConfig struct {
	ID      string `json:"id,omitempty"`
	Label   string `json:"label,omitempty"`
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
}
type lanModelStore struct {
	mu      sync.RWMutex
	path    string
	configs map[string]lanModelConfig
}

func validModelConfig(c lanModelConfig) bool {
	u, err := url.Parse(c.BaseURL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && !strings.ContainsAny(c.BaseURL, "\r\n\t ") && len(c.BaseURL) <= 2048 && strings.TrimSpace(c.Model) == c.Model && c.Model != "" && len(c.Model) <= 256 && len(c.APIKey) <= 8192 && !strings.ContainsAny(c.APIKey, "\r\n")
}

type lanModelFile struct {
	Version int              `json:"version"`
	Configs []lanModelConfig `json:"configs"`
}

func openLanModelStore(dir string) (*lanModelStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &lanModelStore{path: filepath.Join(dir, "model-config.json"), configs: make(map[string]lanModelConfig)}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil || raw == nil {
		return nil, errors.New("invalid saved model configuration")
	}
	var records []lanModelConfig
	if _, ok := raw["version"]; ok {
		var file lanModelFile
		if json.Unmarshal(b, &file) != nil || file.Version != 1 || file.Configs == nil {
			return nil, errors.New("invalid saved model configuration")
		}
		records = file.Configs
	} else {
		var c lanModelConfig
		if json.Unmarshal(b, &c) != nil {
			return nil, errors.New("invalid saved model configuration")
		}
		c.ID, c.Label = lanEngineName, lanEngineName
		records = []lanModelConfig{c}
	}
	for _, c := range records {
		if !validRecord(c) || c.APIKey == "" {
			return nil, errors.New("invalid saved model configuration")
		}
		if _, exists := s.configs[c.ID]; exists {
			return nil, errors.New("invalid saved model configuration")
		}
		s.configs[c.ID] = c
	}
	return s, nil
}
func validRecord(c lanModelConfig) bool {
	return validModelConfig(c) && c.ID != "" && len(c.ID) <= 128 && strings.TrimSpace(c.ID) == c.ID && c.Label != "" && len(c.Label) <= 256 && strings.TrimSpace(c.Label) == c.Label && !strings.ContainsAny(c.ID+c.Label, "\r\n\t")
}
func (s *lanModelStore) get() lanModelConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configs[lanEngineName]
}
func (s *lanModelStore) put(c lanModelConfig) error {
	if c.ID == "" {
		c.ID = lanEngineName
	}
	if c.Label == "" {
		c.Label = c.ID
	}
	_, err := s.save(c, false)
	return err
}

var errInvalidModel = errors.New("invalid model configuration")

func (s *lanModelStore) save(c lanModelConfig, create bool) (lanModelConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if create {
		var id [16]byte
		for {
			if _, err := rand.Read(id[:]); err != nil {
				return lanModelConfig{}, err
			}
			c.ID = hex.EncodeToString(id[:])
			if _, exists := s.configs[c.ID]; !exists {
				break
			}
		}
	} else if previous, ok := s.configs[c.ID]; ok {
		if c.APIKey == "" {
			c.APIKey = previous.APIKey
		}
	} else if c.ID != lanEngineName {
		return lanModelConfig{}, errInvalidModel
	}
	if !validRecord(c) || c.APIKey == "" {
		return lanModelConfig{}, errInvalidModel
	}
	records := make([]lanModelConfig, 0, len(s.configs)+1)
	for id, item := range s.configs {
		if id != c.ID {
			records = append(records, item)
		}
	}
	records = append(records, c)
	b, err := json.Marshal(lanModelFile{Version: 1, Configs: records})
	if err != nil {
		return lanModelConfig{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".model-config-*")
	if err != nil {
		return lanModelConfig{}, err
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
		return lanModelConfig{}, err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return lanModelConfig{}, err
	}
	s.configs[c.ID] = c
	return c, nil
}

type publicLanModel struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"has_api_key"`
}

func publicModel(c lanModelConfig) publicLanModel {
	return publicLanModel{c.ID, c.Label, c.BaseURL, c.Model, c.APIKey != ""}
}
func (s *Server) handleLanModelConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		s.lanSettings.mu.RLock()
		configs := make([]publicLanModel, 0, len(s.lanSettings.configs))
		for _, c := range s.lanSettings.configs {
			configs = append(configs, publicModel(c))
		}
		s.lanSettings.mu.RUnlock()
		sort.Slice(configs, func(i, j int) bool { return configs[i].ID < configs[j].ID })
		writeJSON(w, 200, map[string]any{"configs": configs})
	case http.MethodPut:
		var c lanModelConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&c); err != nil {
			writeError(w, 400, "invalid request body")
			return
		}
		saved, err := s.lanSettings.save(c, c.ID == "")
		if errors.Is(err, errInvalidModel) {
			writeError(w, 400, "invalid model configuration")
			return
		}
		if err != nil {
			writeError(w, 500, "failed to save model configuration")
			return
		}
		writeJSON(w, 200, publicModel(saved))
	default:
		writeError(w, 405, "method not allowed")
	}
}
func (s *Server) lanEngine(name string) (map[string]string, bool) {
	s.lanSettings.mu.RLock()
	c, ok := s.lanSettings.configs[name]
	s.lanSettings.mu.RUnlock()
	if !ok || c.APIKey == "" {
		return nil, false
	}
	return map[string]string{"translate_engine_type": "OpenAI", "openai_base_url": c.BaseURL, "openai_model": c.Model, "openai_api_key": c.APIKey}, true
}
func (s *Server) handleLanEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	s.lanSettings.mu.RLock()
	names := make([]string, 0, len(s.lanSettings.configs))
	for id := range s.lanSettings.configs {
		names = append(names, id)
	}
	s.lanSettings.mu.RUnlock()
	sort.Strings(names)
	writeJSON(w, 200, map[string]any{"engines": names})
}
