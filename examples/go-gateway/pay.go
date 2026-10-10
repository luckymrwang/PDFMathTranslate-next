package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PayService struct {
	cfg      *PayConfig
	store    *OrderStore
	client   *XPayClient
	sessions *paySessions
	createMu sync.Mutex
	locks    orderLocks
}

func (s *Server) payEnabled() bool { return s.pay != nil && s.pay.cfg.Enabled }

func orderView(order PayOrder) map[string]any {
	return map[string]any{"id": order.ID, "state": order.State, "quantity": order.Quantity,
		"unit_price_fen": order.UnitPrice, "total_fen": order.Total,
		"engine": order.Engine, "name": order.Name, "lang_in": order.LangIn, "lang_out": order.LangOut,
		"task_id": order.TaskID, "delivery": order.Delivery, "created_at": order.CreatedAt}
}

func paymentData(order PayOrder, appKey, sessionKey string) map[string]string {
	return map[string]string{"mode": "short_series_goods", "signData": order.SignData,
		"paySig":    payHMAC(appKey, "requestVirtualPayment&"+order.SignData),
		"signature": payHMAC(sessionKey, order.SignData)}
}

func (s *Server) handlePayConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "standard_price_fen": s.pay.cfg.StandardPrice,
		"enhanced_price_fen": s.pay.cfg.EnhancedPrice})
}

// uploadFile is also used for server-side PDF inspection. Never send local
// file paths, translation credentials or detailed upstream errors to clients.
func (s *Server) uploadFile(ctx context.Context, endpoint, path, name string, params map[string]any, idempotencyKey string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	mw := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		part, err := mw.CreateFormFile("file", name)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil && params != nil {
			body, marshalErr := json.Marshal(params)
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = mw.WriteField("data", string(body))
			}
		}
		if err == nil {
			err = mw.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.pdf2zh.BaseURL+endpoint, reader)
	if err != nil {
		reader.Close()
		<-done
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if idempotencyKey != "" {
		req.Header.Set("X-Idempotency-Key", idempotencyKey)
	}
	resp, err := s.pdf2zh.HTTP.Do(req)
	reader.Close()
	streamErr := <-done
	if err != nil {
		return nil, errors.New("python service unavailable")
	}
	defer resp.Body.Close()
	if streamErr != nil {
		return nil, errors.New("PDF transfer failed")
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, errSubmitConflict
	}
	if resp.StatusCode != 200 {
		return nil, errors.New("python rejected PDF or translation settings")
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (s *Server) handlePayOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sessionKey := s.pay.sessions.get(token)
	if sessionKey == "" {
		writeError(w, 401, "请重新登录后支付")
		return
	}
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	if s.pay.store.recentOrderCount(openid) >= 10 {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "操作过于频繁，请一分钟后重试")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 51<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, 400, "PDF 最大 50 MB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "请选择 PDF 文件")
		return
	}
	defer file.Close()
	if header.Size <= 0 || header.Size > 50<<20 || !strings.EqualFold(filepath.Ext(header.Filename), ".pdf") {
		writeError(w, 400, "请选择不超过 50 MB 的 PDF")
		return
	}
	var data struct {
		Engine  string `json:"engine"`
		LangIn  string `json:"lang_in"`
		LangOut string `json:"lang_out"`
	}
	if json.Unmarshal([]byte(r.FormValue("data")), &data) != nil {
		writeError(w, 400, "invalid settings")
		return
	}
	if _, ok := availableEngines()[data.Engine]; !ok {
		writeError(w, 400, "翻译引擎未配置")
		return
	}
	validLanguages := map[string]bool{"zh": true, "en": true, "ja": true, "ko": true, "fr": true, "de": true, "es": true}
	if !validLanguages[data.LangIn] || !validLanguages[data.LangOut] || data.LangIn == data.LangOut {
		writeError(w, 400, "请选择不同的源语言与目标语言")
		return
	}
	staged, err := os.CreateTemp(filepath.Join(s.pay.store.dir, "uploads"), ".upload-*.pdf")
	if err != nil {
		writeError(w, 500, "无法保存文件")
		return
	}
	stagedPath := staged.Name()
	saved := false
	defer func() {
		staged.Close()
		if !saved {
			os.Remove(stagedPath)
		}
	}()
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(staged, hash), file)
	if err == nil {
		err = staged.Sync()
	}
	if err == nil {
		err = staged.Close()
	}
	if err != nil {
		writeError(w, 500, "无法保存文件")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultSubmitTimeout)
	defer cancel()
	raw, err := s.uploadFile(ctx, "/v1/pdf/inspect", stagedPath, header.Filename,
		map[string]any{"lang_in": data.LangIn, "lang_out": data.LangOut, "translate_engine_settings": availableEngines()[data.Engine]}, "")
	var inspected struct {
		Pages int `json:"page_count"`
	}
	if err != nil || json.Unmarshal(raw, &inspected) != nil || inspected.Pages <= 0 || inspected.Pages > 1000 {
		writeError(w, 422, "PDF 无法读取或翻译引擎暂不可用，请检查文件及引擎配置")
		return
	}
	product, price := s.pay.cfg.EnhancedProduct, s.pay.cfg.EnhancedPrice
	if data.Engine == "Google" || data.Engine == "Bing" {
		product, price = s.pay.cfg.StandardProduct, s.pay.cfg.StandardPrice
	}
	if inspected.Pages*price > 10000000 {
		writeError(w, 400, "订单金额超过限额")
		return
	}
	id, err := newTradeNo()
	if err != nil {
		writeError(w, 500, "无法创建订单")
		return
	}
	order := PayOrder{ID: id, OpenID: openid, ProductID: product, UnitPrice: price,
		Quantity: inspected.Pages, Total: inspected.Pages * price, Engine: data.Engine,
		LangIn: data.LangIn, LangOut: data.LangOut, FilePath: stagedPath,
		FileHash: hex.EncodeToString(hash.Sum(nil)), Name: filepath.Base(header.Filename),
		State: "pending", CreatedAt: time.Now().Unix(), Attach: id}
	signData, _ := json.Marshal(map[string]any{"offerId": s.pay.cfg.OfferID,
		"buyQuantity": order.Quantity, "env": 0, "currencyType": "CNY",
		"productId": order.ProductID, "goodsPrice": order.UnitPrice, "outTradeNo": id, "attach": order.Attach})
	order.SignData = string(signData)
	if err := s.pay.store.add(order); err != nil {
		writeError(w, 500, "订单保存失败")
		return
	}
	saved = true
	writeJSON(w, 200, map[string]any{"order": orderView(order), "payData": paymentData(order, s.pay.cfg.AppKey, sessionKey)})
}

func (s *Server) handlePayOrders(w http.ResponseWriter, r *http.Request) {
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	id := strings.TrimPrefix(r.URL.Path, "/pay/orders/")
	if r.URL.Path == "/pay/orders" {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed")
			return
		}
		orders := make([]map[string]any, 0)
		for _, order := range s.pay.store.all() {
			// Unpaid closed orders (cancelled or abandoned payments) are not shown to users.
			if order.OpenID == openid && !(order.State == "closed" && order.WxOrderID == "") {
				orders = append(orders, orderView(order))
			}
		}
		writeJSON(w, 200, map[string]any{"orders": orders})
		return
	}
	start := strings.HasSuffix(id, "/start")
	if start {
		id = strings.TrimSuffix(id, "/start")
	}
	cancelling := strings.HasSuffix(id, "/cancel")
	if cancelling {
		id = strings.TrimSuffix(id, "/cancel")
	}
	order, ok := s.pay.store.get(id)
	if !ok || order.OpenID != openid {
		writeError(w, 404, "订单不存在")
		return
	}
	if cancelling && r.Method == http.MethodPost {
		if err := s.closePendingOrder(r.Context(), id); err != nil {
			writeError(w, 502, "暂时无法确认付款状态，请稍后刷新")
			return
		}
		order, _ = s.pay.store.get(id)
		writeJSON(w, 200, orderView(order))
		return
	}
	if start && r.Method == http.MethodPost {
		taskID, err := s.startPaidOrder(r.Context(), id)
		if err != nil {
			writeError(w, 409, "订单尚未支付或翻译服务不可用，请稍后重试")
			return
		}
		writeJSON(w, 200, map[string]string{"id": taskID})
		return
	}
	if !start && r.Method == http.MethodGet {
		writeJSON(w, 200, orderView(order))
		return
	}
	writeError(w, 405, "method not allowed")
}

func (s *Server) handlePayQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	var req struct {
		ID string `json:"order_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
		writeError(w, 400, "invalid order")
		return
	}
	order, ok := s.pay.store.get(req.ID)
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	if !ok || order.OpenID != openid {
		writeError(w, 404, "订单不存在")
		return
	}
	if order.State == "pending" && time.Now().Unix()-order.LastQuery >= 10 {
		if err := s.reconcileOrder(r.Context(), order.ID); err != nil {
			log.Printf("virtual payment query failed (order=%s): %v", order.ID, err)
			var apiErr *wechatAPIError
			if errors.As(err, &apiErr) {
				writeJSON(w, 502, map[string]any{
					"error":             fmt.Sprintf("微信查单失败（错误码 %d），请检查服务器配置", apiErr.Code),
					"wechat_error_code": apiErr.Code,
				})
				return
			}
			writeError(w, 502, "微信查单暂不可用，请稍后重试")
			return
		}
	}
	order, _ = s.pay.store.get(order.ID)
	writeJSON(w, 200, orderView(order))
}

// Grant only this PDF's translation entitlement. No coins, balance or client
// success flag can grant it. Persist the platform ID before acknowledging.
func (s *Server) grantOrder(id string, verified queriedOrder) error {
	return s.pay.store.update(id, func(order *PayOrder) error {
		if verified.ID != order.ID || verified.EnvType != 1 {
			return errors.New("platform order identity mismatch")
		}
		// An unpaid closed order need not have a platform payment number.
		if verified.Status == 6 && (order.State == "pending" || order.State == "closed") {
			order.State = "closed"
			return nil
		}
		if verified.WxID == "" {
			return errors.New("missing platform payment identity")
		}
		if order.WxOrderID != "" && order.WxOrderID != verified.WxID {
			return errors.New("platform id changed")
		}
		if verified.Status == 5 || verified.Status == 8 {
			order.State = "refunded"
			order.WxOrderID = verified.WxID
			return nil
		}
		if verified.Status != 2 && verified.Status != 3 && verified.Status != 4 {
			return errors.New("order not paid")
		}
		if verified.OrderFee != order.Total || verified.PaidFee != order.Total {
			return errors.New("payment amount mismatch")
		}
		if order.State == "refunded" {
			return errors.New("refunded order cannot be fulfilled")
		}
		order.WxOrderID = verified.WxID
		// A locally closed order the platform reports as paid was paid late; honour it.
		if order.State == "pending" || order.State == "closed" {
			order.State = "paid"
		}
		return nil
	})
}

// closePendingOrder closes an unpaid order after a user cancel or timeout.
// Network failures leave it pending; a later platform payment revives it.
func (s *Server) closePendingOrder(ctx context.Context, id string) error {
	order, ok := s.pay.store.get(id)
	if !ok || order.State != "pending" {
		return nil
	}
	verified, err := s.pay.client.query(ctx, order)
	var apiErr *wechatAPIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Operation == "/xpay/query_order") {
		return err
	}
	if err == nil && verified.Status != 0 && verified.Status != 1 && verified.Status != 6 {
		return s.reconcileOrder(ctx, id)
	}
	return s.pay.store.update(id, func(current *PayOrder) error {
		if current.State == "pending" {
			current.State = "closed"
		}
		return nil
	})
}

func (s *Server) reconcileOrder(ctx context.Context, id string) error {
	order, ok := s.pay.store.get(id)
	if !ok {
		return errors.New("order not found")
	}
	verified, err := s.pay.client.query(ctx, order)
	if err != nil {
		return err
	}
	if verified.ID != order.ID {
		return errors.New("platform order mismatch")
	}
	if err := s.pay.store.update(id, func(current *PayOrder) error { current.LastQuery = time.Now().Unix(); return nil }); err != nil {
		return err
	}
	if verified.Status == 0 || verified.Status == 1 {
		return nil
	}
	if err := s.grantOrder(id, verified); err != nil {
		return err
	}
	order, _ = s.pay.store.get(id)
	if order.State == "refunded" {
		if order.TaskID != "" {
			resp, err := s.pdf2zh.Request(ctx, http.MethodDelete, "/v1/translate/"+order.TaskID)
			if err == nil {
				resp.Body.Close()
			}
		}
		return nil
	}
	if order.State == "closed" {
		return nil
	}
	if !order.Provided {
		if err := s.pay.client.provided(ctx, order); err != nil {
			return err
		}
		if err := s.pay.store.update(id, func(current *PayOrder) error { current.Provided = true; return nil }); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) startPaidOrder(ctx context.Context, id string) (string, error) {
	unlock, _ := s.pay.locks.acquire(id, true)
	defer unlock()
	return s.startPaidOrderLocked(ctx, id)
}

// startPaidOrderLocked requires the caller to hold the order lock.
func (s *Server) startPaidOrderLocked(ctx context.Context, id string) (string, error) {
	for {
		order, ok := s.pay.store.get(id)
		if !ok || (order.State != "paid" && order.State != "submitted") {
			return "", errors.New("unpaid order")
		}
		if order.Delivery == "failed" {
			return "", errors.New("paid translation failed")
		}
		if order.TaskID != "" {
			s.tasks.set(order.TaskID, order.OpenID)
			return order.TaskID, nil
		}
		taskID, err := s.submitPaidOrder(ctx, order)
		if errors.Is(err, errSubmitConflict) {
			// The worker lost the task bound to this attempt's key; retry under a fresh key.
			if err := s.nextPaidAttempt(order.ID); err != nil {
				return "", err
			}
			continue
		}
		return taskID, err
	}
}

func (s *Server) submitPaidOrder(ctx context.Context, order PayOrder) (string, error) {
	settings, ok := availableEngines()[order.Engine]
	if !ok {
		return "", errors.New("engine unavailable")
	}
	file, err := os.Open(order.FilePath)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil || hex.EncodeToString(hash.Sum(nil)) != order.FileHash {
		return "", errors.New("order file changed")
	}
	key := "virtualpay:" + order.ID
	if order.Attempts > 0 {
		key += ":" + strconv.Itoa(order.Attempts)
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSubmitTimeout)
	defer cancel()
	body, err := s.uploadFile(ctx, "/v1/translate", order.FilePath, order.Name,
		map[string]any{"lang_in": order.LangIn, "lang_out": order.LangOut, "qps": 4,
			"skip_image_translation": true, "translate_engine_settings": settings}, key)
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &result) != nil || result.ID == "" {
		return "", errors.New("invalid task response")
	}
	err = s.pay.store.update(order.ID, func(current *PayOrder) error {
		if current.State == "refunded" {
			return errors.New("order refunded")
		}
		current.State = "submitted"
		current.TaskID = result.ID
		return nil
	})
	if err != nil {
		current, _ := s.pay.store.get(order.ID)
		if current.State == "refunded" {
			s.deleteUpstreamTask(ctx, result.ID)
		}
		return "", err
	}
	s.tasks.set(result.ID, order.OpenID)
	return result.ID, nil
}

func (s *Server) runPayReconciliation(ctx context.Context) {
	round := 0
	recoverOrders := func() {
		queryWeChat := round%5 == 0
		round++
		for _, order := range s.pay.store.all() {
			if ctx.Err() != nil {
				return
			}
			if order.Delivery != "" {
				continue
			}
			if order.State == "pending" && time.Since(time.Unix(order.CreatedAt, 0)) > pendingOrderTTL {
				checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := s.closePendingOrder(checkCtx, order.ID); err != nil {
					log.Printf("virtual payment expiry check failed (order=%s): %v", order.ID, err)
				}
				cancel()
				continue
			}
			if queryWeChat && (order.State == "pending" || order.State == "paid" || order.State == "submitted") {
				checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := s.reconcileOrder(checkCtx, order.ID); err != nil {
					log.Printf("virtual payment reconcile failed (order=%s): %v", order.ID, err)
				}
				cancel()
			}
			fresh, _ := s.pay.store.get(order.ID)
			if fresh.State == "paid" || fresh.State == "submitted" {
				advanceCtx, cancel := context.WithTimeout(ctx, defaultSubmitTimeout)
				if err := s.advancePaidOrder(advanceCtx, fresh.ID, true); err != nil {
					log.Printf("paid translation delivery pending (order=%s): %v", fresh.ID, err)
				}
				cancel()
			}
		}
	}
	recoverOrders()
	// Poll often enough to archive results well within the worker's task TTL.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recoverOrders()
		}
	}
}

func (s *Server) taskRefunded(taskID string) bool {
	order, ok := s.paidOrderForTask(taskID)
	return ok && order.State == "refunded"
}

func (s *Server) resumePaid(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultSubmitTimeout)
	defer cancel()
	if _, err := s.startPaidOrder(ctx, id); err != nil {
		log.Printf("paid translation submission failed (order=%s)", id)
	}
}
