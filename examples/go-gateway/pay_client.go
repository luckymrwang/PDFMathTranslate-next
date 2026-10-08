package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type XPayClient struct {
	AppID, AppSecret, AppKey, BaseURL string
	HTTP                              *http.Client
	mu                                sync.Mutex
	token                             string
	expires                           time.Time
}

func newXPayClient(cfg *Config, pay *PayConfig) *XPayClient {
	return &XPayClient{AppID: cfg.AppID, AppSecret: cfg.AppSecret, AppKey: pay.AppKey,
		BaseURL: "https://api.weixin.qq.com", HTTP: &http.Client{Timeout: 10 * time.Second}}
}

func (c *XPayClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	body, _ := json.Marshal(map[string]any{"grant_type": "client_credential",
		"appid": c.AppID, "secret": c.AppSecret, "force_refresh": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/cgi-bin/stable_token", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("cannot construct token request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", errors.New("wechat token service unavailable")
	}
	defer resp.Body.Close()
	var out struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
		ErrCode int    `json:"errcode"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Token == "" || out.Expires <= 60 || out.ErrCode != 0 {
		return "", fmt.Errorf("wechat access token rejected (code %d)", out.ErrCode)
	}
	c.token = out.Token
	c.expires = time.Now().Add(time.Duration(out.Expires-60) * time.Second)
	return c.token, nil
}

func (c *XPayClient) call(ctx context.Context, path string, body []byte, target any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		params := url.Values{"access_token": {token}, "pay_sig": {payHMAC(c.AppKey, path+"&"+string(body))}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path+"?"+params.Encode(), bytes.NewReader(body))
		if err != nil {
			return errors.New("cannot construct xpay request")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return errors.New("wechat payment service unavailable")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != 200 {
			return errors.New("wechat payment service returned invalid response")
		}
		var status struct {
			Code int `json:"errcode"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return err
		}
		if (status.Code == 40001 || status.Code == 42001 || status.Code == 40014) && attempt == 0 {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}
		if status.Code != 0 {
			return fmt.Errorf("wechat payment error code %d", status.Code)
		}
		return json.Unmarshal(raw, target)
	}
	return errors.New("wechat payment authentication failed")
}

type queriedOrder struct {
	ID       string `json:"order_id"`
	WxID     string `json:"wx_order_id"`
	Status   int    `json:"status"`
	OrderFee int    `json:"order_fee"`
	PaidFee  int    `json:"paid_fee"`
	EnvType  int    `json:"env_type"`
}

func (c *XPayClient) query(ctx context.Context, order PayOrder) (queriedOrder, error) {
	body, _ := json.Marshal(map[string]any{"openid": order.OpenID, "env": 0, "order_id": order.ID})
	var result struct {
		Order queriedOrder `json:"order"`
	}
	err := c.call(ctx, "/xpay/query_order", body, &result)
	return result.Order, err
}

func (c *XPayClient) provided(ctx context.Context, order PayOrder) error {
	body, _ := json.Marshal(map[string]any{"order_id": order.ID, "env": 0})
	var result map[string]any
	return c.call(ctx, "/xpay/notify_provide_goods", body, &result)
}
