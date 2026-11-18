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
	errSubmitConflict    = errors.New("paid task requires recovery")
	errNoResult          = errors.New("translation produced no PDF")
	errPaidFailed        = errors.New("paid translation failed permanently")
	errAlreadyTranslated = errors.New("PDF is already a translation output")
)

// msgScanHint is shown when /v1/pdf/inspect flags an image-only scan. Such orders
// are still accepted: GPT orders go to the Mac scan worker, other engines fail and
// are finished manually by the operator.
const msgScanHint = "检测到该 PDF 是扫描件（页面是图片）。标准翻译和 DeepSeek 只能翻译文字，无法识别图片里的内容。\n建议：① 请重新上传非扫描版的 PDF；② 继续使用本文件并选择支持扫描版的 GPT-5.6 或 GPT-6 模型。"

// scanEngines read text from page images, so only they can translate scanned PDFs.
var scanEngines = []string{"GPT-5.6", "GPT-6"}

const msgScanFailed = "该文件是扫描件，系统需要先识别图片中的文字再翻译，处理时间稍长，完成后可直接在本页面查看并下载译文。"

// addScanHint tells the client to suggest an OCR-capable engine; it never blocks the order.
func addScanHint(out map[string]any, scanned bool) {
	out["scanned"] = scanned
	if scanned {
		out["scan_hint"] = msgScanHint
		out["recommended_engines"] = scanEngines
	}
}

// logScannedOrder leaves a trail for operators to find orders that may need manual work.
func logScannedOrder(order PayOrder) {
	if order.Scanned {
		log.Printf("scanned PDF ordered (order=%s, engine=%s, pages=%d, scan_worker=%t)",
			order.ID, order.Engine, order.PageCount(), routeToScanWorker(order))
	}
}

// alreadyTranslated is the Python API's detail/error_type for BabelDOC output files.
const alreadyTranslated = "already_translated"

const msgAlreadyTranslated = "该文件已是翻译后的译文 PDF，请上传原始文档"

// documentTooComplex is the Python API's error_type when BabelDOC rejects a PDF for
// exceeding its parser resource limits. The rejection is deterministic: never retried.
const documentTooComplex = "document_too_complex"

const msgDocumentTooComplex = "该 PDF 结构过于复杂，超出解析上限，无法翻译"

// scannedPDF is the Python API's error_type when BabelDOC rejects a PDF as a scan
// the quote did not flag. Text engines cannot read it, so it is never retried: the
// order goes to the Mac scan worker when its engine can do OCR, and fails at once
// (points refunded, user told to pick a GPT engine) otherwise.
const scannedPDF = "scanned_pdf"

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
	if order.ScanWorker {
		return nil // the Mac scan worker drives it; leases are checked in reconcileScanQueue
	}
	if order.TaskID == "" {
		_, err := s.startPaidOrderLocked(ctx, id)
		return err
	}
	status, err := s.upstreamTaskState(ctx, order.TaskID)
	if err != nil {
		return err
	}
	state, errorType := status.State, status.ErrorType
	switch state {
	case "queued", "running":
		return nil
	case "finished":
		if err := s.archivePaidResult(ctx, order, status.Untranslated); !errors.Is(err, errNoResult) {
			return err
		}
	case "error":
		if errorType == alreadyTranslated || errorType == documentTooComplex {
			s.deleteUpstreamTask(ctx, order.TaskID)
			return s.failPaidOrder(id, errorType)
		}
		if errorType == scannedPDF {
			s.deleteUpstreamTask(ctx, order.TaskID)
			return s.rescueScannedOrder(ctx, id)
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
		log.Printf("paid translation failed after %d attempts; settlement recorded (order=%s)", maxPaidAttempts, id)
		title, next := "付费订单翻译失败，需人工处理", adoptHint(order)
		switch {
		case pointsOnly(order):
			title, next = "积分订单翻译失败（无需处理）", "\n积分会自动退回，用户已被提示"+scanReorderNote(order)
		case order.Scanned:
			title = "扫描件翻译失败，请在 Mac 上立即处理"
		}
		s.notify(title, orderSummary(order)+fmt.Sprintf("\n已自动重试 %d 次仍失败", maxPaidAttempts)+next)
		return errPaidFailed
	}
	return nil
}

// failPaidOrder marks an order whose input can never be translated, skipping retries.
func (s *Server) failPaidOrder(id, reason string) error {
	err := s.pay.store.update(id, func(current *PayOrder) error {
		if current.State == "refunded" {
			return errors.New("order refunded")
		}
		current.Delivery, current.FailReason = "failed", reason
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("paid translation cannot succeed (%s); settlement recorded (order=%s)", reason, id)
	order, _ := s.pay.store.get(id)
	why := "\n原因：" + failReasonText(reason)
	switch {
	case pointsOnly(order):
		next := "\n积分会自动退回"
		if note := scanReorderNote(order); note != "" {
			next += "，用户已被提示" + note
		}
		s.notify("积分订单无法翻译（无需处理）", orderSummary(order)+why+next)
	case order.Scanned:
		s.notify("扫描件翻译失败，请在 Mac 上立即处理", orderSummary(order)+why+adoptHint(order))
	default:
		s.notify("付费订单无法翻译", orderSummary(order)+why)
	}
	return errPaidFailed
}

// failReasonText explains a FailReason code in operator alerts.
func failReasonText(reason string) string {
	switch reason {
	case alreadyTranslated:
		return msgAlreadyTranslated
	case documentTooComplex:
		return msgDocumentTooComplex
	case scannedPDF:
		return "翻译引擎判定为扫描件（页面是图片），文字引擎读不了"
	}
	return reason
}

// rescueScannedOrder handles a PDF BabelDOC rejected as a scan although the quote
// did not flag it. Retrying cannot help, and the user has already paid, so the order
// is marked a scan and goes to the Mac scan worker when its engine can do OCR;
// otherwise it fails right away with the scan explanation and a points refund.
// Callers hold the order lock.
func (s *Server) rescueScannedOrder(ctx context.Context, id string) error {
	err := s.pay.store.update(id, func(current *PayOrder) error {
		if current.State == "refunded" {
			return errors.New("order refunded")
		}
		current.Scanned = true
		if current.TaskID != "" {
			current.TaskHistory = append(current.TaskHistory, current.TaskID)
		}
		current.TaskID = ""
		return nil
	})
	if err != nil {
		return err
	}
	if order, _ := s.pay.store.get(id); routeToScanWorker(order) {
		log.Printf("translation engine rejected a scan; handing it to the scan worker (order=%s)", id)
		_, err := s.startPaidOrderLocked(ctx, id)
		return err
	}
	return s.failPaidOrder(id, scannedPDF)
}

// scanReorderNote completes a points-order alert for a scan on a text-only engine.
func scanReorderNote(order PayOrder) string {
	if order.Scanned && !isScanEngine(order.Engine) {
		return "换 GPT-5.6 / GPT-6 重新下单"
	}
	return ""
}

// adoptHint tells the operator exactly how to hand a failed scan to the Mac.
func adoptHint(order PayOrder) string {
	if !order.Scanned {
		return ""
	}
	return "\n👉 你需要做的事：在 Mac 终端执行下面这条命令，处理完会自动交付用户（用户看到的是“处理时间稍长，完成后可直接下载”）\n" +
		macCommand("adopt "+order.ID)
}

// macCommand formats a scan_worker.py command for the operator's Mac, where
// the repository lives at ~/Documents/www/python3/PDFMathTranslate-next.
func macCommand(args string) string {
	return "cd ~/Documents/www/python3/PDFMathTranslate-next/examples/mac-scan-worker && python3 scan_worker.py " + args
}

// pointsOnly orders are paid entirely with points; a failure gives the points back
// automatically (settleReferrals), so nobody has to refund or rescue them.
func pointsOnly(o PayOrder) bool { return !o.Trial && o.Total == 0 && o.CreditPoints > 0 }

// msgScanPointsFailed tells a points order on a text-only engine how to get a result.
const msgScanPointsFailed = "该文件是扫描件，翻译失败，本单积分已退回；请选择 GPT-5.6 或 GPT-6 重新下单"

func paidFailureMessage(order PayOrder) string {
	// Points go back on the next reconciliation round (about a minute), so say so now.
	if pointsOnly(order) {
		if order.FailReason == alreadyTranslated {
			return msgAlreadyTranslated + "；本单积分已退回"
		}
		if order.FailReason == documentTooComplex {
			return msgDocumentTooComplex + "；本单积分已退回"
		}
		if order.Scanned && !isScanEngine(order.Engine) {
			return msgScanPointsFailed
		}
		return "翻译失败，本单积分已退回"
	}
	if order.Trial {
		if order.FailReason == alreadyTranslated {
			return msgAlreadyTranslated
		}
		if order.FailReason == documentTooComplex {
			return msgDocumentTooComplex
		}
		return "免费试译失败，请联系客服处理"
	}
	if order.FailReason == alreadyTranslated {
		return msgAlreadyTranslated + "；本单请联系客服退款"
	}
	if order.FailReason == documentTooComplex {
		return msgDocumentTooComplex + "；本单请联系客服退款"
	}
	if order.Scanned || order.FailReason == scanFailReason {
		return msgScanFailed
	}
	return "翻译多次失败，请联系客服处理退款"
}

func (s *Server) upstreamTaskState(ctx context.Context, taskID string) (upstreamStatus, error) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodGet, "/v1/translate/"+taskID)
	if err != nil {
		return upstreamStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return upstreamStatus{State: "missing"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return upstreamStatus{}, fmt.Errorf("translation status %d", resp.StatusCode)
	}
	var st upstreamStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return upstreamStatus{}, err
	}
	return st, nil
}

func (s *Server) deleteUpstreamTask(ctx context.Context, taskID string) {
	resp, err := s.pdf2zh.Request(ctx, http.MethodDelete, "/v1/translate/"+taskID)
	if err == nil {
		resp.Body.Close()
	}
}

// resultRetention is how long results and uploaded PDFs are kept (RESULT_RETENTION_HOURS, default 72).
func resultRetention() time.Duration {
	hours, err := strconv.Atoi(os.Getenv("RESULT_RETENTION_HOURS"))
	if err != nil || hours <= 0 {
		hours = 72
	}
	return time.Duration(hours) * time.Hour
}

// expireOrder deletes the files of an order past the retention period and keeps
// the order record for reconciliation. In-progress orders are never touched.
func (s *Server) expireOrder(ctx context.Context, id string) error {
	unlock, ok := s.pay.locks.acquire(id, false)
	if !ok {
		return nil
	}
	defer unlock()
	order, ok := s.pay.store.get(id)
	if !ok || time.Since(time.Unix(max(order.CreatedAt, order.DeliveredAt), 0)) < resultRetention() {
		return nil
	}
	finished := order.Delivery == "archived" || order.Delivery == "failed" ||
		order.State == "closed" || order.State == "refunded"
	hasFiles := order.FilePath != "" || order.MonoFile != "" || order.DualFile != "" ||
		order.MonoKey != "" || order.DualKey != ""
	if !finished || !hasFiles {
		return nil
	}
	for _, key := range []string{order.MonoKey, order.DualKey} {
		if key == "" {
			continue
		}
		if s.storage == nil {
			return errors.New("object storage not configured")
		}
		if err := s.storage.DeleteObject(ctx, key); err != nil {
			return err
		}
	}
	for _, path := range []string{order.MonoFile, order.DualFile, order.FilePath, s.previewPath(order.ID)} {
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return s.pay.store.update(id, func(current *PayOrder) error {
		if current.Delivery == "archived" {
			current.Delivery = "expired"
		}
		current.MonoFile, current.DualFile, current.FilePath = "", "", ""
		current.MonoKey, current.DualKey = "", ""
		return nil
	})
}

func (s *Server) archivePaidResult(ctx context.Context, order PayOrder, untranslated int) error {
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
		current.DeliveredAt = time.Now().Unix()
		current.Untranslated = untranslated
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.uploadArchived(ctx, order.ID); err != nil {
		log.Printf("result upload to object storage deferred (order=%s): %v", order.ID, err)
	}
	if untranslated > 0 {
		s.alertUntranslated(order, untranslated)
	}
	return nil
}

// alertUntranslated tells the operator that a delivered result kept some paragraphs in
// the source language because the translation service failed on them (e.g. Bing
// resetting connections). The order is still delivered; the operator decides whether to
// rerun it, which only retranslates the missing paragraphs thanks to the cache.
func (s *Server) alertUntranslated(order PayOrder, untranslated int) {
	log.Printf("delivered with untranslated paragraphs (order=%s, paragraphs=%d)", order.ID, untranslated)
	s.notify(fmt.Sprintf("译文有 %d 段漏翻，请检查", untranslated), orderSummary(order)+
		fmt.Sprintf("\n翻译服务对 %d 个段落请求失败，这些段落保留了原文；译文已交付给用户。", untranslated)+
		"\n可重跑补翻（已翻译的段落有缓存，只会重翻失败的部分），或改用其他引擎。")
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
	if !s.ordersEnabled() {
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
	if order.Delivery == "expired" && r.Method == http.MethodGet {
		writeError(w, http.StatusGone, "译文已超过保留期，请重新下单")
		return true
	}
	if order.ScanWorker && order.Delivery != "archived" && r.Method == http.MethodGet && sub != "" {
		// Mac scan orders have no Python task to fall back to before delivery.
		writeError(w, http.StatusConflict, "task not finished")
		return true
	}
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
			if errors.Is(err, errObjectNotFound) {
				writeError(w, http.StatusGone, "译文已超过保留期，请重新下单")
				return true
			}
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
	case r.Method == http.MethodGet && sub == "preview" && order.Delivery == "archived":
		s.writePreview(w, r, order)
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
			"error": paidFailureMessage(order)})
		return
	}
	if order.ScanWorker {
		s.writeScanStatus(w, order, taskID)
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
