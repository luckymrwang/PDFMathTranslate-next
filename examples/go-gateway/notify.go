package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Operator alerts (scan worker progress, failed paid orders) go to one chat/push
// webhook, NOTIFY_WEBHOOK_URL. The payload format follows the host: WeCom group bot,
// Feishu/Lark bot, DingTalk bot, Bark or ServerChan; any other URL receives
// {"title","text"} JSON. Alerts are best effort and never block an order.

const notifyPrefix = "【PDF翻译】"

var notifyClient = &http.Client{Timeout: 10 * time.Second}

func (s *Server) notify(title, body string) {
	log.Printf("operator alert: %s", title)
	target := os.Getenv("NOTIFY_WEBHOOK_URL")
	if target == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := notifyRequest(ctx, target, notifyPrefix+title, body)
		if err != nil {
			log.Printf("operator alert not sent: %v", err)
			return
		}
		resp, err := notifyClient.Do(req)
		if err != nil {
			log.Printf("operator alert not sent: %v", err)
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("operator alert rejected: HTTP %d", resp.StatusCode)
		}
	}()
}

func notifyRequest(ctx context.Context, target, title, body string) (*http.Request, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid NOTIFY_WEBHOOK_URL")
	}
	text := title + "\n" + body
	var payload any
	contentType := "application/json"
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "qyapi.weixin.qq.com", host == "oapi.dingtalk.com":
		// WeCom rejects text over 2048 bytes outright; a cut message still arrives.
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": truncateUTF8(text, maxBotTextBytes)}}
	case strings.HasSuffix(host, "feishu.cn"), strings.HasSuffix(host, "larksuite.com"):
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
	case host == "api.day.app":
		payload = map[string]string{"title": title, "body": body, "group": "PDF翻译"}
	case strings.HasSuffix(host, "ftqq.com"):
		form := url.Values{"title": {title}, "desp": {strings.ReplaceAll(body, "\n", "\n\n")}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	default:
		payload = map[string]string{"title": title, "text": body}
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err == nil {
		req.Header.Set("Content-Type", contentType)
	}
	return req, err
}

const maxBotTextBytes = 2000

// truncateUTF8 cuts text to at most limit bytes without splitting a character.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	const more = "\n…（内容过长，已截断）"
	cut := limit - len(more)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + more
}

// orderSummary describes an order for an operator alert. userCode identifies the
// customer: the invite code they see in the mini-program, so a message from them
// can be matched to the order. Empty when the customer is not known.
func orderSummary(o PayOrder, userCode string) string {
	lines := []string{"订单：" + o.ID}
	if userCode != "" {
		lines = append(lines, "用户："+userCode)
	}
	lines = append(lines,
		"文件："+o.Name,
		fmt.Sprintf("页数：%d　引擎：%s　%s→%s", o.PageCount(), o.Engine, o.LangIn, o.LangOut))
	switch {
	case o.Total > 0 && o.CreditPoints > 0:
		lines = append(lines, fmt.Sprintf("金额：¥%.2f + %d 积分", float64(o.Total)/100, o.CreditPoints))
	case o.Total > 0:
		lines = append(lines, fmt.Sprintf("金额：¥%.2f", float64(o.Total)/100))
	case o.CreditPoints > 0:
		lines = append(lines, fmt.Sprintf("积分：%d", o.CreditPoints))
	}
	if o.Scanned {
		lines = append(lines, "类型：扫描件")
	}
	if o.ScanDiscount > 0 {
		lines = append(lines, fmt.Sprintf("优惠：GPT %d 折，省 ¥%.2f", (100-scanDiscountPercent)/10, float64(o.ScanDiscount)/100))
	}
	return strings.Join(lines, "\n")
}

func (s *Server) orderSummary(o PayOrder) string {
	return orderSummary(o, s.customerCode(o.OpenID))
}

// customerCode is the ID an operator can quote back to the customer. Every
// customer that logs in has an invite code; without a stored account the OpenID
// is masked instead of being printed whole.
func (s *Server) customerCode(openid string) string {
	if openid == "" || s.pay == nil || s.pay.referrals == nil || s.pay.referrals.db == nil {
		return ""
	}
	var code string
	if err := s.pay.referrals.db.QueryRow("SELECT code FROM referral_users WHERE openid=?", openid).Scan(&code); err != nil {
		return maskOpenID(openid)
	}
	return code
}

func maskOpenID(openid string) string {
	if len(openid) <= 8 {
		return openid
	}
	return openid[:4] + "…" + openid[len(openid)-4:]
}
