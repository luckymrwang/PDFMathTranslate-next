package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// The free "Bing" product runs on Azure and falls back to scraping Bing when Azure
// refuses (quota used up, key rejected). The fallback keeps orders flowing, which is
// also why nobody notices it: the translation API reports it on /health ("azure") and
// the gateway turns the start and the end of an outage into one alert each.

// azureHealth is the "azure" part of the translation API's /health.
type azureHealth struct {
	State  string `json:"state"` // "ok" or "fallback"
	Since  int64  `json:"since"` // when the outage began (unix seconds)
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// azureWatch remembers the last state the operator was told about.
type azureWatch struct {
	mu    sync.Mutex
	seen  bool
	state string
	since int64
}

// azureWatches holds one azureWatch per translation API client. It is not a Server
// field because the LAN image builds this package with its own Server type.
var azureWatches sync.Map

func watchFor(c *Pdf2zhClient) *azureWatch {
	w, _ := azureWatches.LoadOrStore(c, &azureWatch{})
	return w.(*azureWatch)
}

// azureAlertFresh is how recent an outage must be to be announced after a gateway
// restart; an older one was already reported by the process that saw it start.
const azureAlertFresh = time.Hour

// Health asks the translation API how the Azure engine is doing.
func (c *Pdf2zhClient) Health(ctx context.Context) (azureHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return azureHealth{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return azureHealth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return azureHealth{}, apiError(resp)
	}
	var out struct {
		Azure azureHealth `json:"azure"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return azureHealth{}, err
	}
	return out.Azure, nil
}

// watchAzure alerts once when Azure stops serving and once when it is back. Runs from
// payment reconciliation, so it is polled every minute.
func (s *Server) watchAzure(ctx context.Context, now time.Time) {
	if s.pdf2zh == nil {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	health, err := s.pdf2zh.Health(checkCtx)
	if err != nil {
		logOnce("azure-health:"+err.Error(), "azure health check failed: %v", err)
		return
	}
	if health.State != "fallback" {
		health.State = "ok"
	}

	w := watchFor(s.pdf2zh)
	w.mu.Lock()
	first := !w.seen
	was, wasSince := w.state, w.since
	w.seen, w.state, w.since = true, health.State, health.Since
	w.mu.Unlock()

	switch {
	case health.State == "fallback" && (was != "fallback" || wasSince != health.Since):
		if first && now.Sub(time.Unix(health.Since, 0)) > azureAlertFresh {
			return
		}
		title, body := azureFallbackMessage(health)
		s.notify(title, body)
	case health.State == "ok" && was == "fallback":
		s.notify("Bing 已恢复使用 Azure",
			fmt.Sprintf("中断从 %s 开始，约 %s。现在 Azure 又能正常翻译了，Bing 订单不再走备用线路。",
				time.Unix(wasSince, 0).In(reportZone).Format("01-02 15:04"), outageLength(now, wasSince)))
	}
}

func azureFallbackMessage(h azureHealth) (title, body string) {
	var why string
	switch h.Status {
	case 403:
		title = "Bing 已回退到备用线路：Azure 免费额度已用完"
		why = "Azure 返回 403：免费档（F0）每月 200 万字符，用完后要等下月 1 日（UTC）重置。"
	case 401:
		title = "Bing 已回退到备用线路：Azure 密钥被拒绝"
		why = "Azure 返回 401：密钥无效，或者 AZURE_TRANSLATOR_ENDPOINT 与这把密钥不匹配。"
	default:
		title = fmt.Sprintf("Bing 已回退到备用线路：Azure 不可用（%d）", h.Status)
		why = fmt.Sprintf("Azure 返回 %d。", h.Status)
	}
	body = fmt.Sprintf("开始时间：%s\n%s\n原因原文：%s\n\n订单不受影响：Bing 订单已自动改走网页抓取线路，只是比官方接口稳定性差。"+
		"Azure 会每 30 分钟自动重试一次，恢复后会再通知你一次。",
		time.Unix(h.Since, 0).In(reportZone).Format("01-02 15:04"), why, h.Reason)
	return title, body
}

func outageLength(now time.Time, since int64) string {
	d := now.Sub(time.Unix(since, 0)).Round(time.Minute)
	if d < time.Minute {
		return "不到 1 分钟"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
}
