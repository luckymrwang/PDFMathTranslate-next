package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type deliveryPush struct {
	Event         string `json:"Event" xml:"Event"`
	OpenID        string `json:"OpenId" xml:"OpenId"`
	TradeNo       string `json:"OutTradeNo" xml:"OutTradeNo"`
	Env           *int   `json:"Env" xml:"Env"`
	MchOrderID    string `json:"MchOrderId" xml:"MchOrderId"`
	WxOrderID     string `json:"WxOrderId" xml:"WxOrderId"`
	RetCode       int    `json:"RetCode" xml:"RetCode"`
	WeChatPayInfo struct {
		ID string `json:"MchOrderNo" xml:"MchOrderNo"`
	} `json:"WeChatPayInfo" xml:"WeChatPayInfo"`
	GoodsInfo struct {
		Product  string `json:"ProductId" xml:"ProductId"`
		Quantity int    `json:"Quantity" xml:"Quantity"`
		Attach   string `json:"Attach" xml:"Attach"`
	} `json:"GoodsInfo" xml:"GoodsInfo"`
}

func decodePush(raw []byte, value any) error {
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return json.Unmarshal(raw, value)
	}
	return xml.Unmarshal(raw, value)
}

func pushAck(w http.ResponseWriter, jsonFormat bool, success bool) {
	code, message := 0, "success"
	if !success {
		code, message = 1, "retry"
	}
	if jsonFormat {
		writeJSON(w, 200, map[string]any{"ErrCode": code, "ErrMsg": message})
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, "<xml><ErrCode>"+strconv.Itoa(code)+"</ErrCode><ErrMsg>"+message+"</ErrMsg></xml>")
}

// Only AES-authenticated messages can affect entitlements. Plain URL-validation
// challenges are allowed, but plaintext POST notifications are rejected.
func (s *Server) handlePayNotify(w http.ResponseWriter, r *http.Request) {
	if !s.payEnabled() {
		writeError(w, 503, "virtual payment disabled")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	query := r.URL.Query()
	timestamp, err := strconv.ParseInt(query.Get("timestamp"), 10, 64)
	if err != nil || query.Get("nonce") == "" || timestamp < time.Now().Unix()-300 || timestamp > time.Now().Unix()+300 {
		writeError(w, 403, "invalid notification timestamp")
		return
	}
	encrypted := query.Get("echostr")
	jsonFormat := false
	if r.Method == http.MethodPost {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeError(w, 400, "invalid notification")
			return
		}
		jsonFormat = strings.HasPrefix(strings.TrimSpace(string(raw)), "{")
		var envelope struct {
			Encrypt string `json:"Encrypt" xml:"Encrypt"`
		}
		if decodePush(raw, &envelope) != nil || envelope.Encrypt == "" {
			writeError(w, 403, "encrypted notifications required")
			return
		}
		encrypted = envelope.Encrypt
	}
	if r.Method == http.MethodGet && query.Get("msg_signature") == "" {
		expected := pushSignature(s.pay.cfg.NotifyToken, query.Get("timestamp"), query.Get("nonce"))
		if !hmac.Equal([]byte(query.Get("signature")), []byte(expected)) {
			writeError(w, 403, "invalid signature")
			return
		}
		_, _ = io.WriteString(w, encrypted)
		return
	}
	expected := pushSignature(s.pay.cfg.NotifyToken, query.Get("timestamp"), query.Get("nonce"), encrypted)
	if !hmac.Equal([]byte(query.Get("msg_signature")), []byte(expected)) {
		writeError(w, 403, "invalid signature")
		return
	}
	plain, err := decryptPush(encrypted, s.pay.cfg.AESKey, s.cfg.AppID)
	if err != nil {
		writeError(w, 403, "invalid encrypted message")
		return
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(plain)
		return
	}
	var message deliveryPush
	if decodePush(plain, &message) != nil {
		pushAck(w, jsonFormat, false)
		return
	}
	if message.Event == "xpay_refund_notify" {
		order, ok := s.pay.store.get(message.MchOrderID)
		if !ok || order.OpenID != message.OpenID || order.WxOrderID != message.WxOrderID {
			pushAck(w, jsonFormat, false)
			return
		}
		if message.RetCode != 0 {
			pushAck(w, jsonFormat, true)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		err := s.reconcileOrder(ctx, order.ID)
		fresh, _ := s.pay.store.get(order.ID)
		pushAck(w, jsonFormat, err == nil && fresh.State == "refunded")
		return
	}
	if message.Event != "xpay_goods_deliver_notify" {
		// Do not acknowledge unknown payment events as handled.
		pushAck(w, jsonFormat, false)
		return
	}
	order, ok := s.pay.store.get(message.TradeNo)
	if !ok || order.OpenID != message.OpenID || message.Env == nil || *message.Env != 0 ||
		order.ProductID != message.GoodsInfo.Product || order.Quantity != message.GoodsInfo.Quantity ||
		order.Attach != message.GoodsInfo.Attach {
		pushAck(w, jsonFormat, false)
		return
	}
	// The encrypted notification is authenticated and binds the product/quantity.
	// Also confirm the platform's paid status and amount before granting access.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	verified, err := s.pay.client.query(ctx, order)
	if err == nil && message.WeChatPayInfo.ID != "" && message.WeChatPayInfo.ID != verified.WxID {
		err = context.Canceled
	}
	if err == nil {
		err = s.grantOrder(order.ID, verified)
	}
	if err == nil {
		err = s.pay.store.update(order.ID, func(current *PayOrder) error { current.Provided = true; return nil })
	}
	pushAck(w, jsonFormat, err == nil)
	if err == nil {
		go s.resumePaid(order.ID)
	}
}
