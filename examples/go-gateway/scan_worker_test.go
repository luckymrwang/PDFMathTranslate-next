package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testScanToken = "scan-worker-token-0123456789abcdef"

// alertSink captures operator alerts sent to NOTIFY_WEBHOOK_URL.
type alertSink struct {
	mu     sync.Mutex
	titles []string
}

func newAlertSink(t *testing.T) *alertSink {
	t.Helper()
	sink := &alertSink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Title string }
		json.NewDecoder(r.Body).Decode(&body)
		sink.mu.Lock()
		sink.titles = append(sink.titles, body.Title)
		sink.mu.Unlock()
	}))
	t.Cleanup(server.Close)
	t.Setenv("NOTIFY_WEBHOOK_URL", server.URL+"/hook")
	return sink
}

func (a *alertSink) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, title := range a.titles {
			if strings.Contains(title, want) {
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no alert containing %q; got %q", want, a.titles)
}

// scanOrderFixture stores a paid, scanned order with a real source file.
func scanOrderFixture(t *testing.T, s *Server, engine, output string) PayOrder {
	t.Helper()
	id, _ := newTradeNo()
	content := []byte("%PDF-scanned source " + id)
	path := filepath.Join(t.TempDir(), id+".pdf")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	order := PayOrder{ID: id, OpenID: "owner", Engine: engine, LangIn: "en", LangOut: "zh", Pages: 3,
		Quantity: 3, Total: 150, Name: "scan.pdf", FilePath: path, FileHash: hex.EncodeToString(sum[:]),
		State: "paid", Provided: true, Scanned: true, Output: output, CreatedAt: time.Now().Unix()}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	return order
}

func workerCall(s *Server, method, path string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer "+testScanToken)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.handleScanWorker(w, r)
	return w
}

func pdfUpload(t *testing.T, parts map[string]string) (io.Reader, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range parts {
		part, _ := mw.CreateFormFile(name, name+".pdf")
		part.Write([]byte(content))
	}
	mw.Close()
	return &body, mw.FormDataContentType()
}

func taskStatus(t *testing.T, s *Server, taskID string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/"+taskID, "owner", nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return out
}

func TestScannedGPTOrderIsTranslatedAndDeliveredByMacWorker(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("scanned GPT order must not reach the Python API:", r.URL.Path)
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	order := scanOrderFixture(t, s, "GPT-5.6", "both")

	taskID, err := s.startPaidOrder(context.Background(), order.ID)
	if err != nil || !strings.HasPrefix(taskID, scanTaskPrefix) {
		t.Fatal("order not queued for the scan worker", taskID, err)
	}
	alerts.wait(t, "已交给 Mac")
	if again, _ := s.startPaidOrder(context.Background(), order.ID); again != taskID {
		t.Fatal("restart must keep the queued task", again)
	}
	if st := taskStatus(t, s, taskID); st["state"] != "queued" {
		t.Fatal("waiting order should be queued", st)
	}

	r := httptest.NewRequest("POST", "/worker/scan/claim", nil)
	r.Header.Set("Authorization", "Bearer wrong-token-wrong-token-wrong-token")
	w := httptest.NewRecorder()
	s.handleScanWorker(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatal("worker route accepted a wrong token", w.Code)
	}
	w = workerCall(s, "POST", "/worker/scan/claim", nil, "")
	var job map[string]any
	json.Unmarshal(w.Body.Bytes(), &job)
	if w.Code != 200 || job["id"] != order.ID || job["output"] != "both" || job["lang_out"] != "zh" {
		t.Fatal("claim failed", w.Code, w.Body.String())
	}
	if w = workerCall(s, "POST", "/worker/scan/claim", nil, ""); w.Code != http.StatusNoContent {
		t.Fatal("a claimed order was handed out twice", w.Code)
	}
	if st := taskStatus(t, s, taskID); st["state"] != "running" {
		t.Fatal("claimed order should be running", st)
	}
	w = workerCall(s, "GET", "/worker/scan/"+order.ID+"/source", nil, "")
	if w.Code != 200 || w.Body.String() != "%PDF-scanned source "+order.ID {
		t.Fatal("source download failed", w.Code)
	}
	progress := strings.NewReader(`{"stage":"翻译 2/3 页","progress":60}`)
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/progress", progress, "application/json"); w.Code != 200 {
		t.Fatal("progress rejected", w.Code, w.Body.String())
	}
	if st := taskStatus(t, s, taskID); st["stage"] != "翻译 2/3 页" || st["progress"] != 60.0 {
		t.Fatal("client does not see worker progress", st)
	}

	body, ct := pdfUpload(t, map[string]string{"dual": "not a pdf"})
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/result", body, ct); w.Code != 400 {
		t.Fatal("non-PDF result accepted", w.Code)
	}
	body, ct = pdfUpload(t, map[string]string{"dual": "%PDF-dual"})
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/result", body, ct); w.Code != 400 {
		t.Fatal("result missing the ordered mono PDF accepted", w.Code)
	}
	body, ct = pdfUpload(t, map[string]string{"dual": "%PDF-dual", "mono": "%PDF-mono"})
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/result", body, ct); w.Code != 200 {
		t.Fatal("result rejected", w.Code, w.Body.String())
	}
	alerts.wait(t, "已自动交付")
	delivered, _ := s.pay.store.get(order.ID)
	if delivered.Delivery != "archived" || delivered.DeliveredAt == 0 || delivered.ScanLease != 0 {
		t.Fatal("order not delivered", delivered.Delivery, delivered.DeliveredAt)
	}
	st := taskStatus(t, s, taskID)
	if st["state"] != "finished" || st["dual_url"] == nil || st["mono_url"] == nil {
		t.Fatal("client does not see the delivery", st)
	}
	w = httptest.NewRecorder()
	s.handleTranslateTask(w, ownRequest("GET", "/api/translate/"+taskID+"/dual", "owner", nil))
	if w.Code != 200 || w.Body.String() != "%PDF-dual" {
		t.Fatal("customer cannot download the result", w.Code)
	}
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/result", body, ct); w.Code != http.StatusConflict {
		t.Fatal("a delivered order accepted another result", w.Code)
	}
}

func TestScanWorkerFailureRetriesFailsAndCanBeAdopted(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-6", "dual")
	taskID, err := s.startPaidOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerCall(s, "POST", "/worker/scan/claim", nil, "")
	fail := func(retry bool) {
		payload, _ := json.Marshal(map[string]any{"reason": "model proxy unreachable", "retry": retry})
		if w := workerCall(s, "POST", "/worker/scan/"+order.ID+"/fail", bytes.NewReader(payload), "application/json"); w.Code != 200 {
			t.Fatal("failure report rejected", w.Code, w.Body.String())
		}
	}
	pause, _ := json.Marshal(map[string]any{"reason": "usage limit", "retry": true, "pause": true})
	if w := workerCall(s, "POST", "/worker/scan/"+order.ID+"/fail", bytes.NewReader(pause), "application/json"); w.Code != 200 {
		t.Fatal("pause rejected", w.Code)
	}
	alerts.wait(t, "暂停处理")
	if current, _ := s.pay.store.get(order.ID); current.Attempts != 0 || current.Delivery != "" || current.ScanLease != 0 {
		t.Fatal("a paused order must be re-queued without using an attempt", current.Attempts)
	}
	workerCall(s, "POST", "/worker/scan/claim", nil, "")
	fail(true)
	alerts.wait(t, "已重新排队")
	if current, _ := s.pay.store.get(order.ID); current.Delivery != "" || current.Attempts != 1 || current.ScanLease != 0 {
		t.Fatal("retryable failure should re-queue", current.Delivery, current.Attempts)
	}
	if w := workerCall(s, "POST", "/worker/scan/claim", nil, ""); w.Code != 200 {
		t.Fatal("re-queued order not claimable", w.Code)
	}
	fail(false)
	alerts.wait(t, "请在 Mac 上处理")
	failed, _ := s.pay.store.get(order.ID)
	if failed.Delivery != "failed" || paidFailureMessage(failed) != msgScanFailed {
		t.Fatal("final failure should ask for manual handling", failed.Delivery, paidFailureMessage(failed))
	}
	if st := taskStatus(t, s, taskID); st["state"] != "error" {
		t.Fatal("client should see the failure", st)
	}

	if w := workerCall(s, "POST", "/worker/scan/"+order.ID+"/adopt", nil, ""); w.Code != 200 {
		t.Fatal("adopt rejected", w.Code, w.Body.String())
	}
	adopted, _ := s.pay.store.get(order.ID)
	if adopted.Delivery != "" || adopted.Attempts != 0 || adopted.TaskID != taskID || !adopted.ScanWorker {
		t.Fatal("adopt must re-queue under the task the client polls", adopted)
	}
	if st := taskStatus(t, s, taskID); st["state"] != "queued" {
		t.Fatal("adopted order should be queued again", st)
	}
}

func TestMacSwitchedOffNeverCostsTheOrderAnAttempt(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-5.6", "dual")
	if _, err := s.startPaidOrder(context.Background(), order.ID); err != nil {
		t.Fatal(err)
	}
	// Power cut mid-order: the lease runs out and the order goes back to the queue.
	expire := func() {
		workerCall(s, "POST", "/worker/scan/claim", nil, "")
		s.pay.store.update(order.ID, func(c *PayOrder) error { c.ScanLease = time.Now().Add(-time.Minute).Unix(); return nil })
		s.reconcileScanQueue()
	}
	expire()
	if current, _ := s.pay.store.get(order.ID); current.ScanLease != 0 || current.Attempts != 0 ||
		current.ScanTimeouts != 1 || current.Delivery != "" {
		t.Fatal("an expired lease must re-queue without using an attempt", current.Attempts, current.ScanTimeouts)
	}
	payload := strings.NewReader(`{"stage":"x","progress":1}`)
	if w := workerCall(s, "POST", "/worker/scan/"+order.ID+"/progress", payload, "application/json"); w.Code != http.StatusConflict {
		t.Fatal("a worker that lost its lease must be told to stop", w.Code)
	}
	// Normal shutdown: the worker hands the order back itself.
	workerCall(s, "POST", "/worker/scan/claim", nil, "")
	body := strings.NewReader(`{"reason":"shutdown","interrupted":true}`)
	if w := workerCall(s, "POST", "/worker/scan/"+order.ID+"/fail", body, "application/json"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	alerts.wait(t, "关机或重启")
	if current, _ := s.pay.store.get(order.ID); current.Attempts != 0 || current.Delivery != "" || current.ScanLease != 0 {
		t.Fatal("an interrupted order must be re-queued for free", current.Attempts)
	}
	// An order that keeps killing the worker is given up after maxScanTimeouts.
	for i := 1; i < maxScanTimeouts; i++ {
		expire()
	}
	if current, _ := s.pay.store.get(order.ID); current.Delivery != "failed" || current.ScanTimeouts != maxScanTimeouts {
		t.Fatal("endless crashes must eventually fail the order", current.Delivery, current.ScanTimeouts)
	}
	alerts.wait(t, "多次处理中断")
}

func TestOfflineNotReadyAndRecoveredAlerts(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-5.6", "dual")
	if _, err := s.startPaidOrder(context.Background(), order.ID); err != nil {
		t.Fatal(err)
	}
	s.pay.store.update(order.ID, func(c *PayOrder) error { c.ScanQueuedAt = time.Now().Add(-time.Hour).Unix(); return nil })
	noAlert := func(want string) {
		time.Sleep(100 * time.Millisecond)
		alerts.mu.Lock()
		defer alerts.mu.Unlock()
		for _, title := range alerts.titles {
			if strings.Contains(title, want) {
				t.Fatal("unexpected alert:", title)
			}
		}
	}
	// Sleeping through the night, it promised to be back later: no alert yet.
	s.scanDue.Store(time.Now().Add(10 * time.Minute).Unix())
	s.reconcileScanQueue()
	noAlert("离线")

	// Alive but the model proxy is not running: a specific alert.
	body := strings.NewReader(`{"ready":false,"reason":"model proxy unavailable","idle":60}`)
	if w := workerCall(s, "POST", "/worker/scan/status", body, "application/json"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.reconcileScanQueue()
	alerts.wait(t, "在线但无法接单")

	// Switched off: past its due time. Alerts repeat only every 6 hours.
	s.scanAlert.Store(0)
	s.scanDue.Store(time.Now().Add(-time.Minute).Unix())
	s.reconcileScanQueue()
	alerts.wait(t, "可能已离线")
	alerts.mu.Lock()
	count := len(alerts.titles)
	alerts.mu.Unlock()
	s.reconcileScanQueue()
	time.Sleep(100 * time.Millisecond)
	alerts.mu.Lock()
	if len(alerts.titles) != count {
		t.Fatal("offline alert repeated within 6 hours")
	}
	alerts.mu.Unlock()

	// Switched back on: the first poll announces it and picks up the order.
	if w := workerCall(s, "POST", "/worker/scan/claim", nil, ""); w.Code != 200 {
		t.Fatal("waiting order not handed out after restart", w.Code)
	}
	alerts.wait(t, "已恢复")
}

func TestScannedOrderWithTextEngineStillUsesPythonAndAlerts(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 200, map[string]string{"id": "python-task"})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	order := scanOrderFixture(t, s, "Bing", "dual")
	taskID, err := s.startPaidOrder(context.Background(), order.ID)
	if err != nil || taskID != "python-task" {
		t.Fatal("Bing scan should keep the normal flow", taskID, err)
	}
	alerts.wait(t, "大概率失败")
	if current, _ := s.pay.store.get(order.ID); current.ScanWorker {
		t.Fatal("Bing scan must not be routed to the Mac")
	}
}

func TestRetentionCountsFromDelivery(t *testing.T) {
	t.Setenv("RESULT_RETENTION_HOURS", "72")
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-5.6", "dual")
	result := filepath.Join(t.TempDir(), "dual.pdf")
	os.WriteFile(result, []byte("%PDF-x"), 0o600)
	old := time.Now().Add(-100 * time.Hour).Unix()
	s.pay.store.update(order.ID, func(c *PayOrder) error {
		c.State, c.Delivery, c.DualFile, c.CreatedAt, c.DeliveredAt = "submitted", "archived", result, old, time.Now().Unix()
		return nil
	})
	s.expireOrder(context.Background(), order.ID)
	if current, _ := s.pay.store.get(order.ID); current.Delivery != "archived" {
		t.Fatal("a result delivered today expired by its creation time")
	}
	s.pay.store.update(order.ID, func(c *PayOrder) error { c.DeliveredAt = old; return nil })
	s.expireOrder(context.Background(), order.ID)
	if current, _ := s.pay.store.get(order.ID); current.Delivery != "expired" {
		t.Fatal("old delivery did not expire", current.Delivery)
	}
}

func TestNotifyPayloadMatchesWebhookProvider(t *testing.T) {
	cases := map[string]string{
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k": `"msgtype":"text"`,
		"https://oapi.dingtalk.com/robot/send?access_token=k":    `"msgtype":"text"`,
		"https://open.feishu.cn/open-apis/bot/v2/hook/k":         `"msg_type":"text"`,
		"https://api.day.app/key":                                `"group":"PDF翻译"`,
		"https://sctapi.ftqq.com/key.send":                       "desp=",
		"https://example.com/hook":                               `"text":"body"`,
	}
	for target, want := range cases {
		req, err := notifyRequest(context.Background(), target, "title", "body")
		if err != nil {
			t.Fatal(target, err)
		}
		raw, _ := io.ReadAll(req.Body)
		text := string(raw)
		if strings.HasPrefix(want, "desp") {
			text, _ = url.QueryUnescape(text)
		}
		if !strings.Contains(text, want) || req.Method != http.MethodPost {
			t.Fatalf("%s: payload %s lacks %s", target, raw, want)
		}
	}
	if _, err := notifyRequest(context.Background(), "ftp://x", "t", "b"); err == nil {
		t.Fatal("non-HTTP webhook accepted")
	}
}

func TestLongPollWakesWhenAnOrderIsQueued(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-5.6", "dual")
	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- workerCall(s, "POST", "/worker/scan/claim?wait=60&idle=1500", nil, "") }()
	time.Sleep(150 * time.Millisecond)
	select {
	case w := <-done:
		t.Fatal("long poll returned before any order was queued", w.Code)
	default:
	}
	if due := s.scanDue.Load(); due < start.Add(60*time.Second+1500*time.Second).Unix() {
		t.Fatal("the worker's promised return time ignores its idle period", due-start.Unix())
	}
	if _, err := s.startPaidOrder(context.Background(), order.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		var job map[string]any
		json.Unmarshal(w.Body.Bytes(), &job)
		if w.Code != 200 || job["id"] != order.ID || job["full_qa"] != false {
			t.Fatal("woken long poll did not return the order", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll was not woken by the queued order")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("wake-up was slow")
	}
}

func TestLargeOrderHeldForReviewThenReleased(t *testing.T) {
	t.Setenv("SCAN_WORKER_TOKEN", testScanToken)
	alerts := newAlertSink(t)
	s := testPayServer(t)
	order := scanOrderFixture(t, s, "GPT-6", "dual")
	s.pay.store.update(order.ID, func(c *PayOrder) error { c.Pages = 76; return nil }) // 76 × ¥0.4 > ¥30
	taskID, _ := s.startPaidOrder(context.Background(), order.ID)
	alerts.wait(t, "已交给 Mac")
	w := workerCall(s, "POST", "/worker/scan/claim", nil, "")
	var job map[string]any
	json.Unmarshal(w.Body.Bytes(), &job)
	if job["full_qa"] != true {
		t.Fatal("a ¥30.4 order must get full quality control", w.Body.String())
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("dual", "dual.pdf")
	part.Write([]byte("%PDF-held"))
	mw.WriteField("report", "质检：71 页通过；5 页仍有问题")
	mw.WriteField("hold", "1")
	mw.Close()
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/result", &body, mw.FormDataContentType()); w.Code != 200 {
		t.Fatal("held result rejected", w.Code, w.Body.String())
	}
	alerts.wait(t, "已暂停交付")
	if st := taskStatus(t, s, taskID); st["state"] != "running" || !strings.Contains(fmt.Sprint(st["stage"]), "复核") {
		t.Fatal("customer should see the review stage", st)
	}
	dl := httptest.NewRecorder()
	s.handleTranslateTask(dl, ownRequest("GET", "/api/translate/"+taskID+"/dual", "owner", nil))
	if dl.Code == 200 {
		t.Fatal("a held result must not be downloadable")
	}
	if w = workerCall(s, "POST", "/worker/scan/claim", nil, ""); w.Code != http.StatusNoContent {
		t.Fatal("a held order was handed out again", w.Code)
	}

	s.pay.store.update(order.ID, func(c *PayOrder) error { c.ScanHeldAt = time.Now().Add(-7 * time.Hour).Unix(); return nil })
	s.reconcileScanQueue()
	alerts.wait(t, "等待你的复核")

	upload, ct := pdfUpload(t, map[string]string{"dual": "%PDF-fixed"})
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/release", upload, ct); w.Code != 200 {
		t.Fatal("release rejected", w.Code, w.Body.String())
	}
	alerts.wait(t, "复核后已交付")
	dl = httptest.NewRecorder()
	s.handleTranslateTask(dl, ownRequest("GET", "/api/translate/"+taskID+"/dual", "owner", nil))
	if dl.Code != 200 || dl.Body.String() != "%PDF-fixed" {
		t.Fatal("released order should serve the replacement", dl.Code, dl.Body.String())
	}
	if w = workerCall(s, "POST", "/worker/scan/"+order.ID+"/release", nil, ""); w.Code != http.StatusConflict {
		t.Fatal("a delivered order was released twice", w.Code)
	}
}

func TestSmallOrderSkipsFullQualityControl(t *testing.T) {
	if scanFullQA(PayOrder{Pages: 75}) || !scanFullQA(PayOrder{Pages: 76}) {
		t.Fatal("threshold must be pages × ¥0.4 > ¥30")
	}
}

func TestScanDeliveredAlertOnlyCallsLargeOrdersReviewed(t *testing.T) {
	small, large := PayOrder{Pages: 3}, PayOrder{Pages: 80}
	for _, c := range []struct {
		order  PayOrder
		report string
		want   string
	}{
		{small, "", "扫描件订单已自动交付"},
		{small, "自动质检：3 页规则检查，1 页可疑", "扫描件订单已自动交付（自动质检有发现）"},
		{large, "质检：80 页", "大额扫描件已交付（质检通过）"},
	} {
		if got := scanDeliveredTitle(c.order, c.report); got != c.want {
			t.Fatalf("pages=%d report=%q: got %q, want %q", c.order.Pages, c.report, got, c.want)
		}
	}
}
