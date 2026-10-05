package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// taskRegistry tracks which openid owns which task, so a user can only access
// their own translations. In production back this with Redis/DB.
type taskRegistry struct {
	mu    sync.RWMutex
	tasks map[string]*taskInfo // taskID -> info
}

// taskInfo records ownership and, once archived, the stored object keys.
type taskInfo struct {
	openid   string
	archived bool
	monoKey  string // object key, empty if absent
	dualKey  string // object key, empty if absent
}

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{tasks: make(map[string]*taskInfo)}
}

func (r *taskRegistry) set(taskID, openid string) {
	r.mu.Lock()
	r.tasks[taskID] = &taskInfo{openid: openid}
	r.mu.Unlock()
}

func (r *taskRegistry) owns(taskID, openid string) bool {
	r.mu.RLock()
	t, ok := r.tasks[taskID]
	r.mu.RUnlock()
	return ok && t.openid == openid
}

func (r *taskRegistry) get(taskID string) (*taskInfo, bool) {
	r.mu.RLock()
	t, ok := r.tasks[taskID]
	r.mu.RUnlock()
	return t, ok
}

// setArchived records the object keys produced by archiving a finished task.
func (r *taskRegistry) setArchived(taskID, monoKey, dualKey string) {
	r.mu.Lock()
	if t, ok := r.tasks[taskID]; ok {
		t.archived = true
		t.monoKey = monoKey
		t.dualKey = dualKey
	}
	r.mu.Unlock()
}

func (r *taskRegistry) delete(taskID string) {
	r.mu.Lock()
	delete(r.tasks, taskID)
	r.mu.Unlock()
}

// handleTranslateSubmit proxies POST /api/translate: forwards the multipart
// upload to the Python API and records task ownership for the caller.
func (s *Server) handleTranslateSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultSubmitTimeout)
	defer cancel()

	taskID, err := s.pdf2zh.Submit(ctx, ct, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translation service unavailable")
		return
	}

	s.tasks.set(taskID, openid)
	writeJSON(w, http.StatusOK, map[string]string{"id": taskID})
}

// handleTranslateTask proxies the per-task subroutes under /api/translate/.
//
//	GET    /api/translate/{id}         -> status
//	GET    /api/translate/{id}/stream  -> SSE progress
//	GET    /api/translate/{id}/mono    -> download mono PDF
//	GET    /api/translate/{id}/dual    -> download dual PDF
//	DELETE /api/translate/{id}         -> cancel
func (s *Server) handleTranslateTask(w http.ResponseWriter, r *http.Request) {
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)

	rest := strings.TrimPrefix(r.URL.Path, "/api/translate/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "task id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	taskID := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	if !s.tasks.owns(taskID, openid) {
		// Do not reveal whether the task exists for another user.
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	upstream := "/v1/translate/" + taskID

	switch {
	case r.Method == http.MethodDelete && sub == "":
		resp, err := s.pdf2zh.Request(r.Context(), http.MethodDelete, upstream)
		if err != nil {
			writeError(w, http.StatusBadGateway, "translation service unavailable")
			return
		}
		defer resp.Body.Close()
		s.tasks.delete(taskID)
		relayResponse(w, resp)

	case r.Method == http.MethodGet && sub == "":
		s.proxyGet(w, r, upstream)

	case r.Method == http.MethodGet && sub == "stream":
		s.proxyStream(w, r, upstream+"/stream")

	case r.Method == http.MethodGet && (sub == "mono" || sub == "dual"):
		s.proxyGet(w, r, upstream+"/"+sub)

	case r.Method == http.MethodGet && sub == "result":
		s.handleTranslateResult(w, r, taskID)

	default:
		writeError(w, http.StatusNotFound, "unknown task route")
	}
}

// proxyGet relays a plain upstream GET response (status JSON or PDF download).
func (s *Server) proxyGet(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	resp, err := s.pdf2zh.Request(r.Context(), http.MethodGet, upstreamPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translation service unavailable")
		return
	}
	defer resp.Body.Close()
	relayResponse(w, resp)
}

// proxyStream relays an SSE stream, flushing each chunk to the client.
func (s *Server) proxyStream(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	resp, err := s.pdf2zh.Request(r.Context(), http.MethodGet, upstreamPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translation service unavailable")
		return
	}
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	copyHeader(w.Header(), resp.Header, "Content-Type", "Cache-Control")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.StatusCode)

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// relayResponse copies status code, selected headers and body to the client.
func relayResponse(w http.ResponseWriter, resp *http.Response) {
	copyHeader(w.Header(), resp.Header, "Content-Type", "Content-Disposition", "Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = copyBody(w, resp.Body)
}

// handleTranslateResult archives a finished task's PDFs to object storage and
// returns time-limited presigned download URLs. Archiving happens once per task.
func (s *Server) handleTranslateResult(w http.ResponseWriter, r *http.Request, taskID string) {
	if s.storage == nil {
		writeError(w, http.StatusNotImplemented, "object storage not configured; use /mono and /dual to download directly")
		return
	}

	info, ok := s.tasks.get(taskID)
	if !ok {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// Already archived: just re-issue fresh presigned URLs.
	if info.archived {
		s.writeResultURLs(w, info.monoKey, info.dualKey)
		return
	}

	// Confirm the task has finished before archiving.
	st, err := s.fetchStatus(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "translation service unavailable")
		return
	}
	if st.State != "finished" {
		writeError(w, http.StatusConflict, "task not finished (state="+st.State+")")
		return
	}

	upstream := "/v1/translate/" + taskID
	monoKey, err := s.archiveOne(r.Context(), upstream+"/mono", s.resultKey(info.openid, taskID, "mono"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to archive result")
		return
	}
	dualKey, err := s.archiveOne(r.Context(), upstream+"/dual", s.resultKey(info.openid, taskID, "dual"))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to archive result")
		return
	}
	if monoKey == "" && dualKey == "" {
		writeError(w, http.StatusNotFound, "no result files available")
		return
	}

	s.tasks.setArchived(taskID, monoKey, dualKey)
	s.writeResultURLs(w, monoKey, dualKey)
}

// upstreamStatus is the subset of the Python API status response we need.
type upstreamStatus struct {
	State string `json:"state"`
}

func (s *Server) fetchStatus(ctx context.Context, taskID string) (*upstreamStatus, error) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, "/v1/translate/"+taskID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var st upstreamStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// archiveOne downloads one result PDF from the Python API and uploads it to
// object storage. Returns the object key, or "" if that variant doesn't exist.
func (s *Server) archiveOne(ctx context.Context, upstreamPath, key string) (string, error) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, upstreamPath)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil // variant disabled (e.g. no_mono)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %d", resp.StatusCode)
	}
	if err := s.storage.PutObject(ctx, key, "application/pdf", resp.Body, resp.ContentLength); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Server) resultKey(openid, taskID, kind string) string {
	return fmt.Sprintf("results/%s/%s-%s.pdf", openid, taskID, kind)
}

// writeResultURLs responds with fresh presigned URLs for the archived objects.
func (s *Server) writeResultURLs(w http.ResponseWriter, monoKey, dualKey string) {
	out := map[string]any{
		"expires_in": int(s.storage.cfg.URLTTL.Seconds()),
	}
	if monoKey != "" {
		out["mono_url"] = s.storage.PresignGetURL(monoKey)
	}
	if dualKey != "" {
		out["dual_url"] = s.storage.PresignGetURL(dualKey)
	}
	writeJSON(w, http.StatusOK, out)
}
