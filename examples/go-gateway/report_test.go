package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func (a *alertSink) count(want string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, title := range a.titles {
		if strings.Contains(title, want) {
			n++
		}
	}
	return n
}

func TestPaymentAlertSentOnceWithDailyTotal(t *testing.T) {
	alerts := newAlertSink(t)
	s := testPayServer(t)
	order := PayOrder{ID: "Tpaid", OpenID: "owner", Engine: "GPT-5.6", Pages: 10, Quantity: 7, Total: 210,
		ScanFreePages: 3, ScanDiscount: 90, Scanned: true, State: "pending", CreatedAt: time.Now().Unix()}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	verified := queriedOrder{ID: order.ID, WxID: "wx-1", Status: 2, OrderFee: 210, PaidFee: 210, EnvType: 1}
	// The payment callback and reconciliation both confirm the same payment.
	for i := 0; i < 2; i++ {
		if err := s.grantOrder(order.ID, verified); err != nil {
			t.Fatal(err)
		}
	}
	alerts.wait(t, "新付费订单 ¥2.10")
	time.Sleep(100 * time.Millisecond)
	if n := alerts.count("新付费订单"); n != 1 {
		t.Fatal("a payment must be reported exactly once, got", n)
	}
	paid, _ := s.pay.store.get(order.ID)
	if paid.State != "paid" || paid.PaidAt == 0 {
		t.Fatal("payment time not recorded", paid.State, paid.PaidAt)
	}
	if line := s.todaySales(time.Now()); !strings.Contains(line, "付费 1 单 ¥2.10") {
		t.Fatal("today's total missing the sale:", line)
	}
	if summary := orderSummary(paid); !strings.Contains(summary, "扫描件 8 折，省 ¥0.90") {
		t.Fatal("alert should mention the scan discount:", summary)
	}
}

func TestDailyReportListsOrdersThatNeedTheOperator(t *testing.T) {
	s := testPayServer(t)
	now := time.Date(2026, 11, 15, 9, 30, 0, 0, reportZone)
	yesterday := time.Date(2026, 11, 14, 10, 0, 0, 0, reportZone).Unix()
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	add := func(o PayOrder) PayOrder {
		t.Helper()
		o.OpenID, o.Engine, o.Pages, o.Name = "owner", "GPT-5.6", 10, o.ID+".pdf"
		if o.CreatedAt == 0 {
			o.CreatedAt = yesterday
		}
		if err := s.pay.store.add(o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if title, _ := s.dailyReport(now); title != "每日巡检：一切正常" {
		t.Fatal("an empty day should read all clear:", title)
	}

	add(PayOrder{ID: "Tsold", State: "submitted", Total: 300, PaidAt: yesterday, Delivery: "archived", DeliveredAt: yesterday + 600})
	add(PayOrder{ID: "Tpoints", State: "paid", CreditPoints: 30, Delivery: "archived", DeliveredAt: yesterday})
	failed := add(PayOrder{ID: "Tfailed", State: "submitted", Total: 500, PaidAt: yesterday, Delivery: "failed", Scanned: true})
	add(PayOrder{ID: "Trefunded", State: "refunded", Total: 500, Delivery: "failed"})
	add(PayOrder{ID: "Treturned", State: "paid", CreditPoints: 20, CreditReturned: true, Delivery: "failed", CreatedAt: ago(72 * time.Hour)})
	add(PayOrder{ID: "Tancient", State: "submitted", Total: 100, Delivery: "failed", CreatedAt: ago(10 * 24 * time.Hour)})
	hanging := add(PayOrder{ID: "Thanging", State: "submitted", Total: 100, PaidAt: ago(4 * time.Hour), CreatedAt: ago(4 * time.Hour)})
	add(PayOrder{ID: "Tfresh", State: "submitted", Total: 100, PaidAt: ago(time.Hour), CreatedAt: ago(time.Hour)})
	held := add(PayOrder{ID: "Theld", State: "submitted", Total: 3000, ScanHeld: true, PaidAt: ago(5 * time.Hour), CreatedAt: ago(5 * time.Hour)})
	add(PayOrder{ID: "Ttrial", State: "paid", Trial: true, Delivery: "failed"})

	title, body := s.dailyReport(now)
	if title != "每日巡检：3 单待处理" {
		t.Fatal("wrong headline:", title, "\n", body)
	}
	for _, want := range []string{"昨日（11-14）：付费 2 单 ¥8.00｜积分 1 单｜交付 2 单｜失败 2 单",
		failed.ID, hanging.ID, held.ID, "已等 4 小时", "adopt <订单号>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("report misses %q:\n%s", want, body)
		}
	}
	for _, settled := range []string{"Trefunded", "Treturned", "Tancient", "Tfresh", "Ttrial"} {
		if strings.Contains(body, settled) {
			t.Fatalf("report lists settled or recent order %s:\n%s", settled, body)
		}
	}
	if strings.Count(body, held.ID+"｜") != 1 {
		t.Fatal("a held order belongs to the review list only")
	}
}

func TestDailyReportGoesOutOncePerDay(t *testing.T) {
	alerts := newAlertSink(t)
	t.Setenv("DAILY_REPORT_HOUR", "9")
	s := testPayServer(t)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 15, h, m, 0, 0, reportZone) }

	if day := s.maybeDailyReport(at(8, 59), ""); day != "" {
		t.Fatal("sent before the report hour")
	}
	day := s.maybeDailyReport(at(9, 0), "")
	if day != "2026-11-15" {
		t.Fatal("report not sent at the report hour", day)
	}
	alerts.wait(t, "每日巡检")
	if s.maybeDailyReport(at(15, 0), day) != day {
		t.Fatal("day changed without a new day")
	}
	time.Sleep(100 * time.Millisecond)
	if n := alerts.count("每日巡检"); n != 1 {
		t.Fatal("report sent more than once a day:", n)
	}
	if initialReportDay(at(9, 30)) != "2026-11-15" || initialReportDay(at(8, 0)) != "" {
		t.Fatal("a restart after the report hour must not resend today's report")
	}
	t.Setenv("DAILY_REPORT_HOUR", "off")
	if s.maybeDailyReport(at(9, 0).AddDate(0, 0, 1), day) != day {
		t.Fatal("report sent while disabled")
	}
}

func TestBotTextIsCutWithoutBreakingCharacters(t *testing.T) {
	long := strings.Repeat("失败订单", 400)
	cut := truncateUTF8(long, maxBotTextBytes)
	if len(cut) > maxBotTextBytes || !utf8.ValidString(cut) || !strings.HasSuffix(cut, "已截断）") {
		t.Fatal("bad cut", len(cut), utf8.ValidString(cut))
	}
	if truncateUTF8("短消息", maxBotTextBytes) != "短消息" {
		t.Fatal("short text changed")
	}
}

func TestClosedOrdersStopBeingPolledWeChat(t *testing.T) {
	s := testPayServer(t)
	var queries atomic.Int32
	wx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/xpay/query_order" {
			queries.Add(1)
			writeJSON(w, 200, map[string]any{"errcode": 268490002, "errmsg": "order not exist"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "t", "expires_in": 7200})
	}))
	defer wx.Close()
	s.pay.client = &XPayClient{AppID: defaultAppID, AppSecret: "secret", AppKey: "app-key", BaseURL: wx.URL, HTTP: wx.Client()}

	yesterday, longAgo := time.Now().Add(-24*time.Hour).Unix(), time.Now().Add(-30*24*time.Hour).Unix()
	for _, o := range []PayOrder{
		{ID: "Tfresh", OpenID: "owner", Engine: "GPT-5.6", Total: 300, State: "closed", CreatedAt: yesterday},
		{ID: "Told", OpenID: "owner", Engine: "GPT-5.6", Total: 300, State: "closed", CreatedAt: longAgo},
	} {
		if err := s.pay.store.add(o); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.runPayReconciliation(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for queries.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if queries.Load() == 0 {
		t.Fatal("a recent closed order must still be polled for a late payment")
	}
	if _, polled := onceLogged.Load("closed:Told"); polled {
		t.Fatal("an order past the late payment window must not be polled")
	}
	if _, polled := onceLogged.Load("closed:Tfresh"); !polled {
		t.Fatal("the recent closed order should have been polled (and its failure logged once)")
	}
}
