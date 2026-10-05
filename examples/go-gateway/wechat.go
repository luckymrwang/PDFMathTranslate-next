package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// WeChatClient calls the WeChat mini-program auth API.
type WeChatClient struct {
	AppID     string
	AppSecret string
	HTTP      *http.Client
}

// NewWeChatClient builds a client with a sane HTTP timeout.
func NewWeChatClient(appID, appSecret string) *WeChatClient {
	return &WeChatClient{
		AppID:     appID,
		AppSecret: appSecret,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

// Session is the result of a successful code2session exchange.
// SessionKey must stay server-side and never be sent to the client.
type Session struct {
	OpenID     string
	UnionID    string
	SessionKey string
}

// wxCode2SessionResp is the raw WeChat API response.
type wxCode2SessionResp struct {
	OpenID     string `json:"openid"`
	SessionKey string `json:"session_key"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// Code2Session exchanges a login `code` from wx.login for an openid + session_key.
func (c *WeChatClient) Code2Session(ctx context.Context, code string) (*Session, error) {
	if code == "" {
		return nil, fmt.Errorf("empty code")
	}

	q := url.Values{}
	q.Set("appid", c.AppID)
	q.Set("secret", c.AppSecret)
	q.Set("js_code", code)
	q.Set("grant_type", "authorization_code")

	endpoint := "https://api.weixin.qq.com/sns/jscode2session?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call wechat: %w", err)
	}
	defer resp.Body.Close()

	var out wxCode2SessionResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode wechat response: %w", err)
	}
	if out.ErrCode != 0 {
		// Do not leak secret/appid; surface only WeChat's error code/message.
		return nil, fmt.Errorf("wechat code2session error %d: %s", out.ErrCode, out.ErrMsg)
	}
	if out.OpenID == "" {
		return nil, fmt.Errorf("wechat returned empty openid")
	}

	return &Session{
		OpenID:     out.OpenID,
		UnionID:    out.UnionID,
		SessionKey: out.SessionKey,
	}, nil
}
