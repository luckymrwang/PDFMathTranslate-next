package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Paid translations are retried at most this many times in total before the
// order is marked failed and needs a manual refund.
const maxPaidAttempts = 3

// Unpaid orders older than this are closed by reconciliation.
const pendingOrderTTL = 30 * time.Minute

var (
	errSubmitConflict = errors.New("paid task requires recovery")
	errNoResult       = errors.New("translation produced no PDF")
	errPaidFailed     = errors.New("paid translation failed permanently")
)

// orderLocks serializes work per order so one slow submission never blocks other orders.
type orderLocks struct {
	mu sync.Mutex
	m  map[string]*orderLock
}

type orderLock struct {
	sync.Mutex
	refs int
}

func (l *orderLocks) acquire(id string, wait bool) (func(), bool) {
	l.mu.Lock()
	if l.m == nil {
		l.m = make(map[string]*orderLock)
	}
	entry := l.m[id]
	if entry == nil {
		entry = &orderLock{}
		l.m[id] = entry
	}
	entry.refs++
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
	if wait {
		entry.Lock()
	} else if !entry.TryLock() {
		release()
		return nil, false
	}
	return func() { entry.Unlock(); release() }, true
}

// advancePaidOrder drives a paid order to a durable result: it submits,
// archives finished output into the gateway data dir, and retries tasks the
// worker lost (restart, TTL expiry) or failed. With wait=false it skips an
// order that is already being processed.
func (s *Server) advancePaidOrder(ctx context.Context, id string, wait bool) error {
	unlock, ok := s.pay.locks.acquire(id, wait)
	if !ok {
		return nil
	}
	defer unlock()
	order, ok := s.pay.store.get(id)
	if !ok || order.Delivery != "" || (order.State != "paid" && order.State != "submitted") {
		return nil
	}
	if order.TaskID == "" {
		_, err := s.startPaidOrderLocked(ctx, id)
		return err
	}
	state, err := s.upstreamTaskState(ctx, order.TaskID)
	if err != nil {
		return err
	}
	switch state {
	case "queued", "running":
		return nil
	case "finished":
		if err := s.archivePaidResult(ctx, order); !errors.Is(err, errNoResult) {
			return err
		}
	}
	log.Printf("paid translation task lost or failed, retrying (order=%s, state=%s)", id, state)
	s.deleteUpstreamTask(ctx, order.TaskID)
	if err := s.nextPaidAttempt(id); err != nil {
		return err
	}
	_, err = s.startPaidOrderLocked(ctx, id)
	return err
}

// nextPaidAttempt detaches the current task so the order can be resubmitted
// under a new idempotency key, or marks it failed once attempts run out.
func (s *Server) nextPaidAttempt(id string) error {
	err := s.pay.store.update(id, func(current *PayOrder) error {
		if current.State == "refunded" {
			return errors.New("order refunded")
		}
		if current.Attempts+1 >= maxPaidAttempts {
			current.Delivery = "failed"
			return nil
		}
		if current.TaskID != "" {
			current.TaskHistory = append(current.TaskHistory, current.TaskID)
		}
		current.TaskID = ""
		current.Attempts++
		return nil
	})
	if err != nil {
		return err
	}
	if order, _ := s.pay.store.get(id); order.Delivery == "failed" {
		log.Printf("paid translation failed after %d attempts, manual refund required (order=%s)", maxPaidAttempts, id)
		return errPaidFailed
	}
	return nil
}

func (s *Server) upstreamTaskState(ctx context.Context, taskID string) (string, error) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, "/v1/translate/"+taskID)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "missing", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("translation status %d", resp.StatusCode)
	}
	var st upstreamStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return "", err
	}
	return st.State, nil
}

func (s *Server) deleteUpstreamTask(ctx context.Context, taskID string) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodDelete, "/v1/translate/"+taskID)
	if err == nil {
		resp.Body.Close()
	}
}

func (s *Server) archivePaidResult(ctx context.Context, order PayOrder) error {
	dir := filepath.Join(s.pay.store.dir, "results")
	mono, err := s.saveResult(ctx, order.TaskID, "mono", filepath.Join(dir, order.ID+"-mono.pdf"))
	if err != nil {
		return err
	}
	dual, err := s.saveResult(ctx, order.TaskID, "dual", filepath.Join(dir, order.ID+"-dual.pdf"))
	if err != nil {
		return err
	}
	if mono == "" && dual == "" {
		return errNoResult
	}
	err = s.pay.store.update(order.ID, func(current *PayOrder) error {
		if current.State == "refunded" {
			return errors.New("order refunded")
		}
		current.MonoFile, current.DualFile, current.Delivery = mono, dual, "archived"
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.uploadArchived(ctx, order.ID); err != nil {
		log.Printf("result upload to object storage deferred (order=%s): %v", order.ID, err)
	}
	return nil
}

// uploadArchived moves archived results into object storage, then frees the
// local copies. Callers must hold the order lock.
func (s *Server) uploadArchived(ctx context.Context, id string) error {
	order, ok := s.pay.store.get(id)
	if s.storage == nil || !ok || order.Delivery != "archived" || order.MonoKey != "" || order.DualKey != "" {
		return nil
	}
	mono, err := s.uploadResult(ctx, order.MonoFile, s.resultKey(order.OpenID, order.ID, "mono"))
	if err != nil {
		return err
	}
	dual, err := s.uploadResult(ctx, order.DualFile, s.resultKey(order.OpenID, order.ID, "dual"))
	if err != nil {
		return err
	}
	if err := s.pay.store.update(id, func(current *PayOrder) error {
		current.MonoKey, current.DualKey = mono, dual
		current.MonoFile, current.DualFile = "", ""
		return nil
	}); err != nil {
		return err
	}
	for _, path := range []string{order.MonoFile, order.DualFile} {
		if path != "" {
			_ = os.Remove(path)
		}
	}
	return nil
}

// saveResult copies one result PDF from the worker; "" means the variant does not exist.
func (s *Server) saveResult(ctx context.Context, taskID, kind, dest string) (string, error) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, "/v1/translate/"+taskID+"/"+kind)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: status %d", kind, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".result-*")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	defer os.Remove(path)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func (s *Server) paidOrderForTask(taskID string) (PayOrder, bool) {
	if !s.payEnabled() {
		return PayOrder{}, false
	}
	for _, order := range s.pay.store.all() {
		if order.TaskID == taskID {
			return order, true
		}
		for _, previous := range order.TaskHistory {
			if previous == taskID {
				return order, true
			}
		}
	}
	return PayOrder{}, false
}

// handlePaidTask serves paid-task routes from the durable order record so
// results survive worker restarts and TTL cleanup. Returns false to fall back
// to the plain proxy.
func (s *Server) handlePaidTask(w http.ResponseWriter, r *http.Request, order PayOrder, taskID, sub string) bool {
	switch {
	case r.Method == http.MethodDelete && sub == "":
		writeError(w, http.StatusConflict, "已支付的翻译任务不可取消")
	case r.Method == http.MethodGet && sub == "":
		s.writePaidStatus(w, r, order, taskID)
	case r.Method == http.MethodGet && (sub == "mono" || sub == "dual") && order.Delivery == "archived":
		path, key := order.MonoFile, order.MonoKey
		if sub == "dual" {
			path, key = order.DualFile, order.DualKey
		}
		filename := fmt.Sprintf("attachment; filename=%q", order.ID+"-"+sub+".pdf")
		if path == "" && key != "" && s.storage != nil {
			resp, err := s.storage.GetObject(r.Context(), key)
			if err != nil {
				log.Printf("object storage download failed (order=%s): %v", order.ID, err)
				writeError(w, http.StatusBadGateway, "result storage unavailable")
				return true
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", filename)
			if resp.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			}
			_, _ = io.Copy(w, resp.Body)
			return true
		}
		file, err := os.Open(path)
		if path == "" || err != nil {
			writeError(w, http.StatusNotFound, sub+" PDF not available")
			return true
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			writeError(w, http.StatusNotFound, sub+" PDF not available")
			return true
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", filename)
		http.ServeContent(w, r, "", info.ModTime(), file)
	case r.Method == http.MethodGet && sub == "result":
		s.writePaidResult(w, r, order)
	default:
		return false
	}
	return true
}

func (s *Server) writePaidStatus(w http.ResponseWriter, r *http.Request, order PayOrder, taskID string) {
	switch order.Delivery {
	case "archived":
		out := map[string]any{"id": taskID, "state": "finished", "stage": "", "progress": 100.0,
			"mono_url": nil, "dual_url": nil}
		if order.MonoFile != "" || order.MonoKey != "" {
			out["mono_url"] = "/api/translate/" + taskID + "/mono"
		}
		if order.DualFile != "" || order.DualKey != "" {
			out["dual_url"] = "/api/translate/" + taskID + "/dual"
		}
		writeJSON(w, http.StatusOK, out)
		return
	case "failed":
		writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "state": "error", "progress": 0.0,
			"error": "翻译多次失败，请联系客服处理退款"})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), defaultSubmitTimeout)
		defer cancel()
		if err := s.advancePaidOrder(ctx, order.ID, false); err != nil {
			log.Printf("paid translation delivery pending (order=%s): %v", order.ID, err)
		}
	}()
	recovering := map[string]any{"id": taskID, "state": "queued", "stage": "正在恢复翻译任务", "progress": 0.0}
	if order.TaskID == "" {
		writeJSON(w, http.StatusOK, recovering)
		return
	}
	// Fail fast so the client sees an error instead of its own request timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, "/v1/translate/"+order.TaskID)
	if err != nil {
		log.Printf("translation status unavailable (order=%s, task=%s): %v", order.ID, order.TaskID, err)
		writeError(w, http.StatusGatewayTimeout, "translation service unavailable")
		return
	}
	defer resp.Body.Close()
	var status map[string]any
	if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status) == nil {
		switch status["state"] {
		case "queued", "running":
			status["id"] = taskID
			writeJSON(w, http.StatusOK, status)
			return
		case "finished":
			// Report completion only after the result is durably archived.
			writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "state": "running",
				"stage": "正在保存翻译结果", "progress": 99.0})
			return
		}
	} else if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		log.Printf("translation status error (order=%s, task=%s): HTTP %d", order.ID, order.TaskID, resp.StatusCode)
		writeError(w, http.StatusBadGateway, "translation service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, recovering)
}

func (s *Server) writePaidResult(w http.ResponseWriter, r *http.Request, order PayOrder) {
	if order.Delivery != "archived" {
		writeError(w, http.StatusConflict, "task not finished")
		return
	}
	if s.storage == nil || s.storage.cfg.Proxy {
		writeError(w, http.StatusNotImplemented, "use /mono and /dual to download through the gateway")
		return
	}
	if order.MonoKey == "" && order.DualKey == "" {
		unlock, ok := s.pay.locks.acquire(order.ID, false)
		if ok {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			err := s.uploadArchived(ctx, order.ID)
			cancel()
			unlock()
			if err != nil {
				log.Printf("result upload to object storage failed (order=%s): %v", order.ID, err)
			}
		}
		order, _ = s.pay.store.get(order.ID)
		if order.MonoKey == "" && order.DualKey == "" {
			// Client falls back to downloading through the gateway.
			writeError(w, http.StatusNotImplemented, "result not in object storage yet")
			return
		}
	}
	s.writeResultURLs(w, order.MonoKey, order.DualKey)
}

func (s *Server) uploadResult(ctx context.Context, path, key string) (string, error) {
	if path == "" {
		return "", nil
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if err := s.storage.PutObject(ctx, key, "application/pdf", file, info.Size()); err != nil {
		return "", err
	}
	return key, nil
}
