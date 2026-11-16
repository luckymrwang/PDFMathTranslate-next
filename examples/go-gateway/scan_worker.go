package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Image-only PDFs ordered with an OCR-capable engine skip the Python API, whose
// BabelDOC aborts them ("Scanned PDF detected"). A Mac (the scan worker) polls these
// routes over HTTPS with SCAN_WORKER_TOKEN, runs Apple OCR + a vision model, writes the
// translation onto the page images and uploads the PDFs, which are then delivered
// like any archived order. Only outbound connections are needed on the Mac.
//
//	POST /worker/scan/claim?wait=S&idle=S  next waiting order; with wait the request is
//	                                       held up to S seconds until one is queued (204 when
//	                                       none); idle is how long the worker then sleeps
//	GET  /worker/scan/{id}/source          original PDF
//	POST /worker/scan/{id}/progress        {"stage","progress"}; renews the lease
//	POST /worker/scan/{id}/result          multipart "dual"/"mono" PDFs plus "report" and
//	                                       "hold"; delivers the order, or keeps it for review
//	POST /worker/scan/{id}/release         deliver a held order (optionally new PDFs)
//	POST /worker/scan/{id}/fail            {"reason","retry","pause","interrupted"}
//	POST /worker/scan/status               {"ready","reason","idle"}: the worker is alive but
//	                                       cannot take orders (e.g. model proxy not started)
//	POST /worker/scan/{id}/adopt           hand any undelivered paid order to the worker
//
// Orders worth more than scanQAThresholdFen at scanQAPageFen per page get full
// quality control on the Mac (glossary, checks, page review); the Mac holds them
// for operator review when too many pages still fail.

const (
	scanQAPageFen      = 40   // value per page for the quality-control threshold
	scanQAThresholdFen = 3000 // orders worth more than this get full quality control
	maxScanWait        = 5 * time.Minute
	maxScanIdle        = 2 * time.Hour
	scanHeldReminder   = 6 * time.Hour
	scanOfflineRepeat  = 6 * time.Hour
	maxScanTimeouts    = 5 // lease expiries (worker died mid-order) before giving up
	scanTaskPrefix     = "scan-"
	scanLeaseTTL       = 20 * time.Minute
	scanOfflineAfter   = 10 * time.Minute
	scanFailReason     = "scan_worker"
	maxScanResultBytes = 512 << 20
)

// scanSignal wakes long-polling workers when an order becomes claimable.
type scanSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (s *scanSignal) wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch == nil {
		s.ch = make(chan struct{})
	}
	return s.ch
}

func (s *scanSignal) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
}

// scanFullQA reports whether an order gets full quality control on the Mac.
func scanFullQA(o PayOrder) bool { return o.PageCount()*scanQAPageFen > scanQAThresholdFen }

func scanWorkerToken() string { return os.Getenv("SCAN_WORKER_TOKEN") }

// scanWorkerEnabled requires a long random token: these routes expose order files.
func scanWorkerEnabled() bool { return len(scanWorkerToken()) >= 32 }

func isScanEngine(engine string) bool {
	for _, e := range scanEngines {
		if e == engine {
			return true
		}
	}
	return false
}

// routeToScanWorker reports whether an order is (or should be) handled by the Mac.
func routeToScanWorker(o PayOrder) bool {
	return o.ScanWorker || (o.Scanned && !o.Trial && isScanEngine(o.Engine) && scanWorkerEnabled())
}

// scanWaiting reports whether the worker may still deliver the order (held ones included).
func scanWaiting(o PayOrder) bool {
	return o.ScanWorker && o.Delivery == "" && o.State == "submitted" && !o.CreditReturned
}

// scanClaimable reports whether the worker may take the order now.
func scanClaimable(o PayOrder, now int64) bool {
	return scanWaiting(o) && !o.ScanHeld && o.ScanLease < now
}

// queueScanOrder hands a paid order to the scan worker. Callers hold the order lock.
func (s *Server) queueScanOrder(order PayOrder) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	taskID := scanTaskPrefix + hex.EncodeToString(raw)
	err := s.pay.store.update(order.ID, func(c *PayOrder) error {
		if c.State == "refunded" {
			return errors.New("order refunded")
		}
		c.State, c.TaskID, c.ScanWorker, c.ScanQueuedAt = "submitted", taskID, true, time.Now().Unix()
		c.ScanLease, c.ScanStage, c.ScanProgress = 0, "", 0
		return nil
	})
	if err != nil {
		return "", err
	}
	s.tasks.set(taskID, order.OpenID)
	s.scanSig.wake()
	s.notify("扫描件订单已交给 Mac 处理", s.orderSummary(order)+qaNote(order))
	return taskID, nil
}

// qaNote tells the operator whether the order gets full quality control.
func qaNote(o PayOrder) string {
	if scanFullQA(o) {
		return fmt.Sprintf("\n大额订单（按 ¥%.2f 计）：统一术语 + 逐页质检", float64(o.PageCount()*scanQAPageFen)/100)
	}
	return ""
}

func (s *Server) writeScanStatus(w http.ResponseWriter, order PayOrder, taskID string) {
	state, stage := "queued", "排队等待识别扫描件"
	if order.ScanHeld {
		writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "state": "running",
			"stage": "质量复核中，完成后自动交付", "progress": 99.0})
		return
	}
	if order.ScanLease > time.Now().Unix() {
		state, stage = "running", order.ScanStage
		if stage == "" {
			stage = "正在识别扫描件"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "state": state, "stage": stage,
		"progress": order.ScanProgress})
}

func (s *Server) handleScanWorker(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !scanWorkerEnabled() || !s.ordersEnabled() ||
		subtle.ConstantTimeCompare([]byte(token), []byte(scanWorkerToken())) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/worker/scan/")
	if rest == "claim" && r.Method == http.MethodPost {
		s.claimScanOrder(w, r)
		return
	}
	if rest == "status" && r.Method == http.MethodPost {
		s.scanStatus(w, r)
		return
	}
	id, action, _ := strings.Cut(rest, "/")
	if _, ok := s.pay.store.get(id); !ok || id == "" {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	switch {
	case action == "source" && r.Method == http.MethodGet:
		s.serveScanSource(w, r, id)
	case action == "progress" && r.Method == http.MethodPost:
		s.scanProgress(w, r, id)
	case action == "result" && r.Method == http.MethodPost:
		s.scanResult(w, r, id)
	case action == "fail" && r.Method == http.MethodPost:
		s.scanFail(w, r, id)
	case action == "adopt" && r.Method == http.MethodPost:
		s.scanAdopt(w, r, id)
	case action == "release" && r.Method == http.MethodPost:
		s.scanRelease(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "unknown worker route")
	}
}

func (s *Server) claimScanOrder(w http.ResponseWriter, r *http.Request) {
	seconds := func(name string, limit time.Duration) time.Duration {
		v, _ := strconv.Atoi(r.URL.Query().Get(name))
		return min(max(time.Duration(v)*time.Second, 0), limit)
	}
	wait, idle := seconds("wait", maxScanWait), seconds("idle", maxScanIdle)
	start := time.Now()
	s.scanPoll.Store(start.Unix())
	s.scanReason.Store("")
	s.scanRecovered()
	// The worker promises to be back by then; only later is it considered offline.
	s.scanDue.Store(start.Add(wait + idle + 5*time.Minute).Unix())
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		woken := s.scanSig.wait() // taken before looking, so a wake in between is not lost
		if claimed, ok := s.claimNext(); ok {
			log.Printf("scan worker claimed order %s", claimed.ID)
			writeJSON(w, http.StatusOK, map[string]any{"id": claimed.ID, "task_id": claimed.TaskID,
				"name": claimed.Name, "pages": claimed.PageCount(), "engine": claimed.Engine,
				"lang_in": claimed.LangIn, "lang_out": claimed.LangOut, "output": outputOrDefault(claimed.Output),
				"attempt": claimed.Attempts + 1, "lease_seconds": int(scanLeaseTTL.Seconds()),
				"full_qa": scanFullQA(claimed)})
			return
		}
		select {
		case <-woken:
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) claimNext() (PayOrder, bool) {
	now := time.Now()
	var waiting []PayOrder
	for _, o := range s.pay.store.all() {
		if scanClaimable(o, now.Unix()) {
			waiting = append(waiting, o)
		}
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].ScanQueuedAt < waiting[j].ScanQueuedAt })
	for _, candidate := range waiting {
		// Wait for the lock: wake-ups are sent while the queuing code still holds it.
		unlock, _ := s.pay.locks.acquire(candidate.ID, true)
		var claimed PayOrder
		err := s.pay.store.update(candidate.ID, func(c *PayOrder) error {
			if !scanClaimable(*c, now.Unix()) {
				return errors.New("no longer waiting")
			}
			c.ScanLease = now.Add(scanLeaseTTL).Unix()
			c.ScanStage, c.ScanProgress = "已开始处理", 0
			claimed = *c
			return nil
		})
		unlock()
		if err == nil {
			return claimed, true
		}
	}
	return PayOrder{}, false
}

func (s *Server) serveScanSource(w http.ResponseWriter, r *http.Request, id string) {
	order, _ := s.pay.store.get(id)
	if !scanWaiting(order) || order.FilePath == "" {
		writeError(w, http.StatusConflict, "order is not waiting for the scan worker")
		return
	}
	file, err := os.Open(order.FilePath)
	if err != nil {
		writeError(w, http.StatusGone, "source PDF no longer available")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusGone, "source PDF no longer available")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("X-File-Sha256", order.FileHash)
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (s *Server) scanProgress(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Stage    string  `json:"stage"`
		Progress float64 `json:"progress"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "invalid progress")
		return
	}
	if utf8.RuneCountInString(in.Stage) > 60 {
		in.Stage = string([]rune(in.Stage)[:60])
	}
	in.Progress = min(max(in.Progress, 0), 99)
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	now := time.Now()
	err := s.pay.store.update(id, func(c *PayOrder) error {
		if !scanWaiting(*c) || c.ScanHeld || c.ScanLease < now.Unix() {
			return errors.New("lease lost")
		}
		c.ScanLease = now.Add(scanLeaseTTL).Unix()
		c.ScanStage, c.ScanProgress = in.Stage, in.Progress
		return nil
	})
	if err != nil {
		// The worker must stop: the order was delivered, refunded or re-queued.
		writeError(w, http.StatusConflict, "lease lost")
		return
	}
	s.scanPoll.Store(now.Unix())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// receiveScanPDFs stores the uploaded "dual"/"mono" PDFs the order needs under their
// final names and returns them with the small text fields ("report", "hold").
func (s *Server) receiveScanPDFs(w http.ResponseWriter, r *http.Request, order PayOrder, required bool) (map[string]string, map[string]string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxScanResultBytes)
	fields, saved := map[string]string{}, map[string]string{}
	mr, err := r.MultipartReader()
	if err != nil {
		if required {
			writeError(w, http.StatusBadRequest, "expected multipart upload")
			return nil, nil, false
		}
		return saved, fields, true // release without new files
	}
	dir := filepath.Join(s.pay.store.dir, "results")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store result")
		return nil, nil, false
	}
	fail := func(code int, msg string) (map[string]string, map[string]string, bool) {
		for _, path := range saved {
			os.Remove(path)
		}
		writeError(w, code, msg)
		return nil, nil, false
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(http.StatusBadRequest, "upload interrupted")
		}
		kind := part.FormName()
		switch {
		case kind == "report" || kind == "hold":
			raw, _ := io.ReadAll(io.LimitReader(part, 16<<10))
			fields[kind] = string(raw)
		case (kind == "dual" || kind == "mono") && saved[kind] == "":
			path, err := saveScanPart(dir, part)
			if err != nil {
				part.Close()
				return fail(http.StatusBadRequest, kind+": "+err.Error())
			}
			saved[kind] = path
		}
		part.Close()
	}
	output := outputOrDefault(order.Output)
	if len(saved) > 0 || required {
		if (output != "mono" && saved["dual"] == "") || (output != "dual" && saved["mono"] == "") {
			return fail(http.StatusBadRequest, "missing PDF for output "+output)
		}
	}
	for kind, path := range saved { // keep only the formats the customer ordered
		if output != "both" && output != kind {
			os.Remove(path)
			delete(saved, kind)
		}
	}
	for kind, path := range saved {
		dest := filepath.Join(dir, order.ID+"-"+kind+".pdf")
		if err := os.Rename(path, dest); err != nil {
			return fail(http.StatusInternalServerError, "cannot store result")
		}
		saved[kind] = dest
	}
	return saved, fields, true
}

// scanResult stores the uploaded PDFs and delivers the order, or holds it for
// operator review when the worker's quality control asks for it.
func (s *Server) scanResult(w http.ResponseWriter, r *http.Request, id string) {
	order, _ := s.pay.store.get(id)
	if !scanWaiting(order) || order.ScanHeld {
		writeError(w, http.StatusConflict, "order is not waiting for the scan worker")
		return
	}
	saved, fields, ok := s.receiveScanPDFs(w, r, order, true)
	if !ok {
		return
	}
	hold := fields["hold"] == "1"
	report := strings.TrimSpace(fields["report"])
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	now := time.Now().Unix()
	err := s.pay.store.update(id, func(c *PayOrder) error {
		if !scanWaiting(*c) || c.ScanHeld {
			return errors.New("order is no longer waiting")
		}
		c.MonoFile, c.DualFile, c.ScanReport = saved["mono"], saved["dual"], report
		c.ScanLease, c.FailReason = 0, ""
		if hold {
			c.ScanHeld, c.ScanHeldAt, c.ScanStage, c.ScanProgress = true, now, "", 99
			return nil
		}
		c.Delivery, c.DeliveredAt, c.ScanStage, c.ScanProgress = "archived", now, "", 100
		return nil
	})
	if err != nil {
		for _, path := range saved {
			os.Remove(path)
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	current, _ := s.pay.store.get(id)
	if hold {
		s.notify("大额扫描件质检未通过，已暂停交付，等待你的复核", s.orderSummary(current)+"\n"+report+
			"\n检查后在 Mac 终端执行（二选一）：\n"+macCommand("release "+id)+"  ← 直接交付\n"+
			macCommand("release "+id+" --dual 修正后.pdf")+"  ← 换上修正后的文件再交付")
		writeJSON(w, http.StatusOK, map[string]string{"delivery": "held"})
		return
	}
	s.deliverScanOrder(id)
	title := "扫描件订单已自动交付"
	if report != "" {
		title = "大额扫描件已交付（质检通过）"
	}
	delivered, _ := s.pay.store.get(id)
	s.notify(title, s.orderSummary(delivered)+optionalLine(report))
	writeJSON(w, http.StatusOK, map[string]string{"delivery": "archived"})
}

func optionalLine(text string) string {
	if text == "" {
		return ""
	}
	return "\n" + text
}

// deliverScanOrder moves a delivered order's results to object storage.
func (s *Server) deliverScanOrder(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.uploadArchived(ctx, id); err != nil {
		log.Printf("result upload to object storage deferred (order=%s): %v", id, err)
	}
}

// scanRelease delivers an order held for review, with its held PDFs or replacements.
func (s *Server) scanRelease(w http.ResponseWriter, r *http.Request, id string) {
	order, _ := s.pay.store.get(id)
	if !scanWaiting(order) || !order.ScanHeld {
		writeError(w, http.StatusConflict, "order is not held for review")
		return
	}
	saved, _, ok := s.receiveScanPDFs(w, r, order, false)
	if !ok {
		return
	}
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	err := s.pay.store.update(id, func(c *PayOrder) error {
		if !scanWaiting(*c) || !c.ScanHeld {
			return errors.New("order is not held for review")
		}
		if len(saved) > 0 {
			c.MonoFile, c.DualFile = saved["mono"], saved["dual"]
		}
		c.ScanHeld, c.ScanProgress = false, 100
		c.Delivery, c.DeliveredAt = "archived", time.Now().Unix()
		return nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.deliverScanOrder(id)
	delivered, _ := s.pay.store.get(id)
	note := "使用 Mac 生成的文件"
	if len(saved) > 0 {
		note = "使用替换后的文件"
	}
	s.notify("复核后已交付", s.orderSummary(delivered)+"\n"+note)
	writeJSON(w, http.StatusOK, map[string]string{"delivery": "archived"})
}

// saveScanPart writes one uploaded PDF to a private temporary file in dir.
func saveScanPart(dir string, part io.Reader) (string, error) {
	tmp, err := os.CreateTemp(dir, ".scan-*.pdf")
	if err != nil {
		return "", errors.New("cannot store result")
	}
	path := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	head := make([]byte, 5)
	n, _ := io.ReadFull(part, head)
	if string(head[:n]) != "%PDF-" {
		return "", errors.New("not a PDF")
	}
	if _, err := tmp.Write(head); err != nil {
		return "", errors.New("cannot store result")
	}
	if _, err := io.Copy(tmp, part); err != nil {
		return "", errors.New("upload interrupted")
	}
	if err := tmp.Sync(); err != nil {
		return "", errors.New("cannot store result")
	}
	ok = true
	return path, nil
}

// scanFail re-queues a failed attempt, or fails the order for manual handling once
// attempts run out or the worker says retrying is pointless.
func (s *Server) scanFail(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Reason string `json:"reason"`
		Retry  bool   `json:"retry"`
		// Pause hands the order back without using an attempt: the Mac itself is
		// unable to work (model quota exhausted, proxy down), not the order.
		Pause bool `json:"pause"`
		// Interrupted: the Mac shut down or restarted mid-order; no attempt is used.
		Interrupted bool `json:"interrupted"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "invalid failure report")
		return
	}
	if utf8.RuneCountInString(in.Reason) > 300 {
		in.Reason = string([]rune(in.Reason)[:300])
	}
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	failed := false
	err := s.pay.store.update(id, func(c *PayOrder) error {
		if !scanWaiting(*c) || c.ScanHeld {
			return errors.New("order is not waiting for the scan worker")
		}
		c.ScanLease, c.ScanStage, c.ScanProgress = 0, "", 0
		if in.Pause || in.Interrupted {
			return nil
		}
		c.Attempts++
		if !in.Retry || c.Attempts >= maxPaidAttempts {
			c.Delivery, c.FailReason, failed = "failed", scanFailReason, true
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	order, _ := s.pay.store.get(id)
	if !failed {
		s.scanSig.wake()
	}
	if in.Interrupted {
		log.Printf("scan worker interrupted on order %s: %s", id, in.Reason)
		s.notify("Mac 关机或重启，扫描件订单已退回队列", s.orderSummary(order)+
			"\n不计失败次数，Mac 开机后会接着处理（已完成的页面不会重做）\n"+in.Reason)
	} else if in.Pause {
		s.notify("Mac 暂停处理扫描件", s.orderSummary(order)+"\n订单已退回队列（不计失败次数）\n原因："+in.Reason)
	} else if failed {
		s.notify("扫描件订单处理失败，请在 Mac 上处理", s.orderSummary(order)+"\n原因："+in.Reason+
			"\n👉 处理：在 Mac 终端执行 "+macCommand("adopt "+id))
	} else {
		s.notify("扫描件订单处理出错，已重新排队", s.orderSummary(order)+
			fmt.Sprintf("\n第 %d 次尝试失败：%s", order.Attempts, in.Reason))
	}
	writeJSON(w, http.StatusOK, map[string]string{"delivery": order.Delivery})
}

// scanAdopt hands an undelivered paid order (typically a scan ordered with Bing that
// failed) to the scan worker, replacing manual delivery.
func (s *Server) scanAdopt(w http.ResponseWriter, r *http.Request, id string) {
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	order, _ := s.pay.store.get(id)
	switch {
	case order.State != "paid" && order.State != "submitted":
		writeError(w, http.StatusConflict, "order is not paid (state "+order.State+")")
		return
	case order.CreditReturned:
		writeError(w, http.StatusConflict, "points were already returned for this order")
		return
	case order.Delivery != "" && order.Delivery != "failed":
		writeError(w, http.StatusConflict, "order already "+order.Delivery)
		return
	case order.FilePath == "":
		writeError(w, http.StatusGone, "source PDF no longer available")
		return
	}
	if _, err := os.Stat(order.FilePath); err != nil {
		writeError(w, http.StatusGone, "source PDF no longer available")
		return
	}
	if order.TaskID != "" && !order.ScanWorker {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		s.deleteUpstreamTask(ctx, order.TaskID)
		cancel()
	}
	if order.TaskID == "" {
		raw := make([]byte, 16)
		rand.Read(raw)
		order.TaskID = scanTaskPrefix + hex.EncodeToString(raw)
	}
	err := s.pay.store.update(id, func(c *PayOrder) error {
		if c.State == "refunded" {
			return errors.New("order refunded")
		}
		// Keep the task ID the client already polls; status now comes from the worker.
		c.State, c.TaskID, c.ScanWorker, c.ScanQueuedAt = "submitted", order.TaskID, true, time.Now().Unix()
		c.Delivery, c.FailReason, c.Attempts = "", "", 0
		c.ScanLease, c.ScanStage, c.ScanProgress = 0, "", 0
		c.ScanHeld, c.ScanReport, c.ScanTimeouts = false, "", 0
		return nil
	})
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if order.ScanHeld { // re-processing replaces the held results
		for _, path := range []string{order.MonoFile, order.DualFile} {
			if path != "" {
				os.Remove(path)
			}
		}
		s.pay.store.update(id, func(c *PayOrder) error { c.MonoFile, c.DualFile = "", ""; return nil })
	}
	s.tasks.set(order.TaskID, order.OpenID)
	s.scanSig.wake()
	log.Printf("order %s handed to the scan worker", id)
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "task_id": order.TaskID})
}

// reconcileScanQueue re-queues orders whose worker went silent, reminds the operator
// of orders held for review and alerts when queued orders wait for a worker that
// did not come back when it said it would. Runs from payment reconciliation.
func (s *Server) reconcileScanQueue() {
	now := time.Now()
	oldestWait := int64(0)
	for _, o := range s.pay.store.all() {
		if !scanWaiting(o) {
			continue
		}
		if o.ScanHeld {
			last, _ := s.scanReminded.Load(o.ID)
			since := o.ScanHeldAt
			if t, ok := last.(int64); ok {
				since = t
			}
			if now.Unix()-since >= int64(scanHeldReminder.Seconds()) {
				s.scanReminded.Store(o.ID, now.Unix())
				hours := (now.Unix() - o.ScanHeldAt) / 3600
				s.notify(fmt.Sprintf("提醒：扫描件订单已暂停交付 %d 小时，等待你的复核", hours),
					s.orderSummary(o)+"\n放行（处理完这单就结束）：在 Mac 终端执行 "+macCommand("release "+o.ID))
			}
			continue
		}
		if o.ScanLease == 0 {
			if oldestWait == 0 || o.ScanQueuedAt < oldestWait {
				oldestWait = o.ScanQueuedAt
			}
			continue
		}
		if o.ScanLease >= now.Unix() {
			continue
		}
		unlock, ok := s.pay.locks.acquire(o.ID, false)
		if !ok {
			continue
		}
		failed := false
		err := s.pay.store.update(o.ID, func(c *PayOrder) error {
			if !scanWaiting(*c) || c.ScanHeld || c.ScanLease == 0 || c.ScanLease >= now.Unix() {
				return errors.New("lease renewed")
			}
			// Usually the Mac was switched off: not the order's fault, so no attempt is
			// used. Only an order that keeps killing the worker is eventually given up.
			c.ScanTimeouts++
			c.ScanLease, c.ScanStage, c.ScanProgress = 0, "", 0
			if c.ScanTimeouts >= maxScanTimeouts {
				c.Delivery, c.FailReason, failed = "failed", scanFailReason, true
			}
			return nil
		})
		unlock()
		if err != nil {
			continue
		}
		fresh, _ := s.pay.store.get(o.ID)
		if failed {
			s.notify("扫描件订单多次处理中断，请在 Mac 上处理", s.orderSummary(fresh)+
				fmt.Sprintf("\n已中断 %d 次（Mac 关机或处理程序崩溃）", fresh.ScanTimeouts)+
				"\n👉 处理：在 Mac 终端执行 "+macCommand("adopt "+fresh.ID))
		} else {
			s.scanSig.wake()
			log.Printf("scan lease expired, re-queued (order=%s, timeouts=%d)", o.ID, fresh.ScanTimeouts)
		}
	}
	// Offline: orders have waited a while and the worker is past the time it promised
	// to poll again (long poll + its night-time sleep), or it said it cannot work.
	due, lastPoll := s.scanDue.Load(), s.scanPoll.Load()
	reason, _ := s.scanReason.Load().(string)
	offline := now.Unix() > due
	if oldestWait == 0 || now.Unix()-oldestWait <= int64(scanOfflineAfter.Seconds()) || (!offline && reason == "") ||
		now.Unix()-s.scanAlert.Load() < int64(scanOfflineRepeat.Seconds()) {
		return
	}
	s.scanAlert.Store(now.Unix())
	s.scanOffline.Store(now.Unix())
	waited := fmt.Sprintf("有扫描件订单已等待 %d 分钟。", (now.Unix()-oldestWait)/60)
	if !offline {
		s.notify("Mac 在线但无法接单", waited+"原因："+reason)
		return
	}
	since := "从未连接"
	if lastPoll > 0 {
		since = "最后一次连接：" + time.Unix(lastPoll, 0).In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
	}
	s.notify("Mac 处理机可能已离线", waited+since+
		"\n请检查 Mac 是否开机并已登录（FileVault 开启时重启后需要输入密码），以及网络和模型代理。")
}

// scanStatus records a worker that is alive but cannot take orders right now.
func (s *Server) scanStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
		Idle   int    `json:"idle"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if utf8.RuneCountInString(in.Reason) > 200 {
		in.Reason = string([]rune(in.Reason)[:200])
	}
	now := time.Now()
	idle := min(max(time.Duration(in.Idle)*time.Second, 0), maxScanIdle)
	s.scanPoll.Store(now.Unix())
	s.scanDue.Store(now.Add(idle + 5*time.Minute).Unix())
	if in.Ready {
		s.scanReason.Store("")
		s.scanRecovered()
	} else {
		if in.Reason == "" {
			in.Reason = "未说明原因"
		}
		s.scanReason.Store(in.Reason)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// scanRecovered tells the operator a worker reported offline is back.
func (s *Server) scanRecovered() {
	if s.scanOffline.Swap(0) == 0 {
		return
	}
	s.scanAlert.Store(0)
	waiting := 0
	now := time.Now().Unix()
	for _, o := range s.pay.store.all() {
		if scanClaimable(o, now) {
			waiting++
		}
	}
	s.notify("Mac 已恢复，开始处理扫描件", fmt.Sprintf("队列中等待处理：%d 单", waiting))
}
