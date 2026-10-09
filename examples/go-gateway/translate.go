package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
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
	if s.payEnabled() {
		writeError(w, http.StatusPaymentRequired, "请通过已支付订单启动翻译")
		return
	}

	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	// Limit the complete request and never trust model settings supplied by the
	// client. ParseMultipartForm spills larger files to a temporary file.
	r.Body = http.MaxBytesReader(w, r.Body, 51<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid or oversized upload (max PDF 50 MB)")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "PDF file is required")
		return
	}
	defer file.Close()
	if header.Size > 50<<20 || !strings.EqualFold(filepath.Ext(header.Filename), ".pdf") {
		writeError(w, http.StatusBadRequest, "PDF must be at most 50 MB")
		return
	}
	magic := make([]byte, 5)
	if _, err := io.ReadFull(file, magic); err != nil || string(magic) != "%PDF-" {
		writeError(w, http.StatusBadRequest, "invalid PDF file")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusBadRequest, "cannot read PDF file")
		return
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(r.FormValue("data")), &data); err != nil || data == nil {
		writeError(w, http.StatusBadRequest, "invalid translation settings")
		return
	}
	engine, _ := data["engine"].(string)
	settings, ok := availableEngines()[engine]
	if !ok {
		writeError(w, http.StatusBadRequest, "translation engine is unavailable")
		return
	}
	// Only known, safe options are passed to Python. In particular, any
	// translate_engine_settings/API keys from the client are discarded.
	forward := map[string]any{
		"lang_in": data["lang_in"], "lang_out": data["lang_out"],
		"translate_engine_settings": settings,
		"skip_image_translation":    true,
	}
	if skip, ok := data["skip_image_translation"].(bool); ok {
		forward["skip_image_translation"] = skip
	}
	if pages, ok := data["pages"].(string); ok && pages != "" {
		forward["pages"] = pages
	}
	if qps, ok := data["qps"].(float64); ok && qps >= 1 && qps <= 20 {
		forward["qps"] = int(qps)
	}
	encoded, _ := json.Marshal(forward)
	reader, writer := io.Pipe()
	defer reader.Close()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		part, err := multipartWriter.CreateFormFile("file", header.Filename)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil {
			err = multipartWriter.WriteField("data", string(encoded))
		}
		if err == nil {
			err = multipartWriter.Close()
		}
		_ = writer.CloseWithError(err)
	}()
	ctx, cancel := context.WithTimeout(r.Context(), defaultSubmitTimeout)
	defer cancel()

	taskID, err := s.pdf2zh.Submit(ctx, multipartWriter.FormDataContentType(), reader)
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
	if s.taskRefunded(taskID) {
		writeError(w, http.StatusForbidden, "订单已退款，翻译权益已撤销")
		return
	}
	if order, ok := s.paidOrderForTask(taskID); ok && s.handlePaidTask(w, r, order, taskID, sub) {
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
