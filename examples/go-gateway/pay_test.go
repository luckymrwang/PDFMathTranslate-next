package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVirtualPaymentSignatures(t *testing.T) {
	// Independent Python hmac/hashlib fixture following the supplied skill.
	body := `{"offerId":"test-offer","buyQuantity":2,"env":0,"currencyType":"CNY","productId":"pdf-standard","goodsPrice":10,"outTradeNo":"T123456789","attach":"T123456789"}`
	order := PayOrder{SignData: body}
	signed := paymentData(order, "app-key", "session-key")
	if signed["paySig"] != "946d1e3aec2a25549681f624c38a712c5e7b0a5af656897a552bb1d868e3f46b" ||
		signed["signature"] != "5f75264fab085321030fad01a9c7e51ba44629a35e238cf4012143a5f7c7d434" ||
		signed["signData"] != body || signed["mode"] != "short_series_goods" {
		t.Fatal("signature fixture mismatch")
	}
}

func testPayServer(t *testing.T) *Server {
	t.Helper()
	store, err := openOrderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: &Config{AppID: defaultAppID}, tasks: newTaskRegistry(),
		pay: &PayService{cfg: &PayConfig{Enabled: true, OfferID: "offer", AppKey: "app-key",
			NotifyToken: "notify-token", AESKey: strings.TrimRight(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), "="),
			StandardProduct: "standard", EnhancedProduct: "enhanced", StandardPrice: 10, EnhancedPrice: 50},
			store: store, sessions: &paySessions{keys: make(map[string]paySession)}}}
}

func ownRequest(method, path, owner string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	return r.WithContext(context.WithValue(r.Context(), ctxKeyOpenID, owner))
}

func TestServerPricingPaymentGateAndRestart(t *testing.T) {
	s := testPayServer(t)
	var submissions atomic.Int32
	python := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pdf/inspect" {
			writeJSON(w, 200, map[string]int{"page_count": 16})
			return
		}
		if r.URL.Path != "/v1/translate" || !strings.HasPrefix(r.Header.Get("X-Idempotency-Key"), "virtualpay:T") {
			t.Error("missing paid request idempotency key")
		}
		submissions.Add(1)
		writeJSON(w, 200, map[string]string{"id": "task-1"})
	}))
	defer python.Close()
	s.pdf2zh = NewPdf2zhClient(python.URL)
	var paid atomic.Bool
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/stable_token" {
			writeJSON(w, 200, map[string]any{"access_token": "access-token", "expires_in": 7200})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("pay_sig") != payHMAC("app-key", r.URL.Path+"&"+string(body)) {
			t.Error("query signature/body mismatch")
		}
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		if r.URL.Path == "/xpay/query_order" {
			if input["order_id"] == nil || input["out_trade_no"] != nil || input["env"] != float64(0) {
				t.Error("incorrect query parameters")
			}
			status := 1
			if paid.Load() {
				status = 2
			}
			writeJSON(w, 200, map[string]any{"errcode": 0, "order": map[string]any{
				"order_id": input["order_id"], "wx_order_id": "wx-1", "env_type": 1,
				"status": status, "order_fee": 160, "paid_fee": 160}})
			return
		}
		writeJSON(w, 200, map[string]int{"errcode": 0})
	}))
	defer wx.Close()
	s.pay.client = &XPayClient{AppID: defaultAppID, AppSecret: "dummy", AppKey: "app-key", BaseURL: wx.URL, HTTP: wx.Client()}
	s.pay.sessions.set("token", "session-key")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "test.pdf")
	_, _ = part.Write([]byte("%PDF-1.7\n"))
	_ = mw.WriteField("data", `{"engine":"Bing","lang_in":"en","lang_out":"zh","quantity":1,"goodsPrice":1}`)
	_ = mw.Close()
	r := ownRequest("POST", "/pay/order", "owner", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.handlePayOrder(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	orders := s.pay.store.all()
	if len(orders) != 1 || orders[0].Quantity != 16 || orders[0].Total != 160 {
		t.Fatal("client tampered with pricing")
	}
	id := orders[0].ID
	if len(id) != 31 {
		t.Fatal("invalid business order length")
	}
	if _, err := s.startPaidOrder(context.Background(), id); err == nil || submissions.Load() != 0 {
		t.Fatal("unpaid order translated")
	}
	w = httptest.NewRecorder()
	s.handleTranslateSubmit(w, ownRequest("POST", "/api/translate", "owner", nil))
	if w.Code != http.StatusPaymentRequired {
		t.Fatal("direct translation bypassed payment")
	}
	w = httptest.NewRecorder()
	s.handlePayOrders(w, ownRequest("GET", "/pay/orders/"+id, "other-owner", nil))
	if w.Code != 404 {
		t.Fatal("cross-account order exposed")
	}
	paid.Store(true)
	if err := s.reconcileOrder(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.startPaidOrder(context.Background(), id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if submissions.Load() != 1 {
		t.Fatal("duplicate translation submission")
	}
	reloaded, err := openOrderStore(s.pay.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	order, ok := reloaded.get(id)
	if !ok || order.State != "submitted" || order.WxOrderID != "wx-1" || order.TaskID != "task-1" {
		t.Fatal("order not durable")
	}
}

func TestGrantRejectsMismatchDuplicateAndRefundReplay(t *testing.T) {
	s := testPayServer(t)
	order := PayOrder{ID: "T12345678", OpenID: "owner", State: "pending", Total: 50}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	v := queriedOrder{ID: order.ID, WxID: "wx-1", Status: 2, EnvType: 1, OrderFee: 50, PaidFee: 1}
	if s.grantOrder(order.ID, v) == nil {
		t.Fatal("wrong amount accepted")
	}
	v.PaidFee = 50
	if err := s.grantOrder(order.ID, v); err != nil {
		t.Fatal(err)
	}
	if err := s.grantOrder(order.ID, v); err != nil {
		t.Fatal("same order must be idempotent")
	}
	other := PayOrder{ID: "T87654321", State: "pending", Total: 50}
	_ = s.pay.store.add(other)
	v.ID = other.ID
	if s.grantOrder(other.ID, v) == nil {
		t.Fatal("platform order reused")
	}
	v.ID = order.ID
	v.Status = 5
	if err := s.grantOrder(order.ID, v); err != nil {
		t.Fatal(err)
	}
	v.Status = 2
	if s.grantOrder(order.ID, v) == nil {
		t.Fatal("refunded order resurrected")
	}
	if err := s.grantOrder(other.ID, queriedOrder{ID: other.ID, Status: 6, EnvType: 1}); err != nil {
		t.Fatal("unpaid closed order must not require a platform payment number")
	}
	closed, _ := s.pay.store.get(other.ID)
	if closed.State != "closed" {
		t.Fatal("closed status not saved")
	}
}

func encryptedFixture(t *testing.T, text, keyString, appID string) string {
	t.Helper()
	key, err := decodeAESKey(keyString)
	if err != nil {
		t.Fatal(err)
	}
	message := append(bytes.Repeat([]byte{1}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(message[16:], uint32(len(text)))
	message = append(message, []byte(text)...)
	message = append(message, []byte(appID)...)
	padding := 32 - len(message)%32
	message = append(message, bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(message, message)
	return base64.StdEncoding.EncodeToString(message)
}

func TestEncryptedPushAuthentication(t *testing.T) {
	s := testPayServer(t)
	encrypted := encryptedFixture(t, "hello", s.pay.cfg.AESKey, s.cfg.AppID)
	decoded, err := decryptPush(encrypted, s.pay.cfg.AESKey, s.cfg.AppID)
	if err != nil || string(decoded) != "hello" {
		t.Fatal("AES message could not be decrypted")
	}
	if _, err := decryptPush(encrypted, s.pay.cfg.AESKey, "other-app"); err == nil {
		t.Fatal("wrong appid accepted")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	params := url.Values{"timestamp": {timestamp}, "nonce": {"test"}}
	params.Set("msg_signature", pushSignature(s.pay.cfg.NotifyToken, timestamp, "test", encrypted))
	params.Set("echostr", encrypted)
	w := httptest.NewRecorder()
	s.handlePayNotify(w, httptest.NewRequest("GET", "/pay/notify?"+params.Encode(), nil))
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatal("URL validation failed")
	}
	params.Set("msg_signature", "forged")
	w = httptest.NewRecorder()
	s.handlePayNotify(w, httptest.NewRequest("GET", "/pay/notify?"+params.Encode(), nil))
	if w.Code != 403 {
		t.Fatal("invalid signature accepted")
	}
	w = httptest.NewRecorder()
	s.handlePayNotify(w, httptest.NewRequest("POST", "/pay/notify?"+params.Encode(), strings.NewReader("<xml><Event>xpay_goods_deliver_notify</Event></xml>")))
	if w.Code != 403 {
		t.Fatal("unsigned plaintext notification accepted")
	}
}

func TestOrderWriteFailureDoesNotGrant(t *testing.T) {
	s := testPayServer(t)
	_ = s.pay.store.add(PayOrder{ID: "T12345678", State: "pending", Total: 50})
	dir := filepath.Join(s.pay.store.dir, "orders")
	if err := os.Rename(dir, dir+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	err := s.grantOrder("T12345678", queriedOrder{ID: "T12345678", WxID: "wx-1", Status: 2, EnvType: 1, OrderFee: 50, PaidFee: 50})
	order, _ := s.pay.store.get("T12345678")
	if err == nil || order.State != "pending" {
		t.Fatal("granted despite failed durable write")
	}
}

func TestEncryptedDeliveryIsIdempotent(t *testing.T) {
	s := testPayServer(t)
	order := PayOrder{ID: "T12345678", OpenID: "owner", State: "submitted", Total: 50,
		ProductID: "standard", Quantity: 5, Attach: "T12345678", WxOrderID: "wx-1", TaskID: "task-1"}
	_ = s.pay.store.add(order)
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/stable_token" {
			writeJSON(w, 200, map[string]any{"access_token": "token", "expires_in": 7200})
			return
		}
		writeJSON(w, 200, map[string]any{"errcode": 0, "order": map[string]any{
			"order_id": order.ID, "wx_order_id": "wx-1", "env_type": 1,
			"status": 4, "order_fee": 50, "paid_fee": 50}})
	}))
	defer wx.Close()
	s.pay.client = &XPayClient{AppKey: "app-key", BaseURL: wx.URL, HTTP: wx.Client()}
	for _, product := range []string{"standard", "standard", "wrong"} {
		text := fmt.Sprintf("<xml><Event>xpay_goods_deliver_notify</Event><OpenId>owner</OpenId><OutTradeNo>%s</OutTradeNo><Env>0</Env><WeChatPayInfo><MchOrderNo>wx-1</MchOrderNo></WeChatPayInfo><GoodsInfo><ProductId>%s</ProductId><Quantity>5</Quantity><Attach>%s</Attach></GoodsInfo></xml>", order.ID, product, order.ID)
		encrypted := encryptedFixture(t, text, s.pay.cfg.AESKey, defaultAppID)
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		params := url.Values{"timestamp": {timestamp}, "nonce": {"test"}, "msg_signature": {pushSignature(s.pay.cfg.NotifyToken, timestamp, "test", encrypted)}}
		w := httptest.NewRecorder()
		s.handlePayNotify(w, httptest.NewRequest("POST", "/pay/notify?"+params.Encode(), strings.NewReader("<xml><Encrypt>"+encrypted+"</Encrypt></xml>")))
		expected := "<ErrCode>0</ErrCode>"
		if product == "wrong" {
			expected = "<ErrCode>1</ErrCode>"
		}
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatal("unexpected delivery acknowledgement", w.Body.String())
		}
	}
}
