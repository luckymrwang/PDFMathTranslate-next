package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The daily report is the safety net behind the instant alerts: an alert can be lost
// (webhook down, gateway restarting), an order can hang without ever failing, and a
// failed order is easy to forget once its one alert scrolled away. Once a day it sums
// up yesterday and lists every order that still needs the operator.

// reportZone is the operator's clock; the server itself may run on UTC.
var reportZone = time.FixedZone("UTC+8", 8*3600)

const (
	defaultReportHour = 9
	// stuckAfter: a paid order neither delivered nor failed by then needs a look.
	// Long scans finish well within it; an offline Mac has its own alerts.
	stuckAfter = 3 * time.Hour
	// unresolvedDays bounds how long a failed, unrefunded order keeps being listed.
	unresolvedDays = 7
	// reportListLimit keeps each list short enough for a chat message.
	reportListLimit = 5
)

// reportHour is DAILY_REPORT_HOUR (0-23, UTC+8); "off" disables the report.
func reportHour() int {
	value := strings.TrimSpace(os.Getenv("DAILY_REPORT_HOUR"))
	if value == "off" {
		return -1
	}
	if hour, err := strconv.Atoi(value); err == nil && hour >= 0 && hour <= 23 {
		return hour
	}
	return defaultReportHour
}

// initialReportDay treats today's report as sent when the gateway starts after the
// report hour, so a restart or deploy does not send it twice.
func initialReportDay(now time.Time) string {
	local := now.In(reportZone)
	if hour := reportHour(); hour >= 0 && local.Hour() >= hour {
		return local.Format("2006-01-02")
	}
	return ""
}

// maybeDailyReport sends the report once per day at the report hour and returns the
// day it last went out.
func (s *Server) maybeDailyReport(now time.Time, last string) string {
	hour := reportHour()
	local := now.In(reportZone)
	day := local.Format("2006-01-02")
	if hour < 0 || day == last || local.Hour() < hour {
		return last
	}
	title, body := s.dailyReport(now)
	s.notify(title, body)
	return day
}

func paidTime(o PayOrder) int64 {
	if o.PaidAt > 0 {
		return o.PaidAt
	}
	return o.CreatedAt
}

// sold reports a completed sale: cash paid, or a points order, and not refunded.
func sold(o PayOrder) bool {
	return !o.Trial && (o.State == "paid" || o.State == "submitted") && (o.Total > 0 || o.CreditPoints > 0)
}

// unresolvedFailure is a failed order nobody has settled: cash not refunded yet, or
// points not given back.
func unresolvedFailure(o PayOrder, now time.Time) bool {
	if o.Trial || o.Delivery != "failed" || o.State == "refunded" || (o.Total == 0 && o.CreditReturned) {
		return false
	}
	return now.Sub(time.Unix(o.CreatedAt, 0)) < unresolvedDays*24*time.Hour
}

func stuck(o PayOrder, now time.Time) bool {
	return sold(o) && o.Delivery == "" && !o.ScanHeld && now.Sub(time.Unix(paidTime(o), 0)) > stuckAfter
}

// salesLine sums the sales in [from, to).
func salesLine(orders []PayOrder, from, to time.Time) string {
	cash, cashFen, points := 0, 0, 0
	for _, o := range orders {
		at := time.Unix(paidTime(o), 0)
		if !sold(o) || at.Before(from) || !at.Before(to) {
			continue
		}
		if o.Total > 0 {
			cash++
			cashFen += o.Total
		} else {
			points++
		}
	}
	return fmt.Sprintf("付费 %d 单 ¥%.2f｜积分 %d 单", cash, float64(cashFen)/100, points)
}

// todaySales is the running total added to every new-sale alert.
func (s *Server) todaySales(now time.Time) string {
	local := now.In(reportZone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, reportZone)
	return "今日累计：" + salesLine(s.pay.store.all(), start, start.AddDate(0, 0, 1))
}

func (s *Server) dailyReport(now time.Time) (string, string) {
	orders := s.pay.store.all()
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt < orders[j].CreatedAt })
	local := now.In(reportZone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, reportZone)
	yesterday := today.AddDate(0, 0, -1)

	delivered, failed := 0, 0
	var unresolved, hanging, held []PayOrder
	for _, o := range orders {
		if o.Trial {
			continue
		}
		if at := time.Unix(o.DeliveredAt, 0); o.DeliveredAt > 0 && !at.Before(yesterday) && at.Before(today) {
			delivered++
		}
		if at := time.Unix(o.CreatedAt, 0); o.Delivery == "failed" && !at.Before(yesterday) && at.Before(today) {
			failed++
		}
		switch {
		case unresolvedFailure(o, now):
			unresolved = append(unresolved, o)
		case o.ScanHeld && o.Delivery == "" && sold(o):
			held = append(held, o)
		case stuck(o, now):
			hanging = append(hanging, o)
		}
	}

	lines := []string{fmt.Sprintf("昨日（%s）：%s｜交付 %d 单｜失败 %d 单",
		yesterday.Format("01-02"), salesLine(orders, yesterday, today), delivered, failed)}
	lines = appendSection(lines, fmt.Sprintf("❗ 失败未处理（近 %d 天，需退款或人工交付）", unresolvedDays), unresolved, now)
	lines = appendSection(lines, fmt.Sprintf("⏳ 已付款超过 %d 小时仍未交付", int(stuckAfter.Hours())), hanging, now)
	lines = appendSection(lines, "🔍 质检暂停，等待你复核", held, now)
	if hasScan(unresolved) {
		lines = append(lines, "", "扫描件在 Mac 终端执行："+macCommand("adopt <订单号>"))
	}

	pending := len(unresolved) + len(hanging) + len(held)
	title := "每日巡检：一切正常"
	if pending > 0 {
		title = fmt.Sprintf("每日巡检：%d 单待处理", pending)
	}
	return title, strings.Join(lines, "\n")
}

func appendSection(lines []string, heading string, orders []PayOrder, now time.Time) []string {
	if len(orders) == 0 {
		return lines
	}
	lines = append(lines, "", fmt.Sprintf("%s：%d 单", heading, len(orders)))
	for i, o := range orders {
		if i == reportListLimit {
			lines = append(lines, fmt.Sprintf("…另有 %d 单，见网关日志", len(orders)-i))
			break
		}
		lines = append(lines, "• "+reportLine(o, now))
	}
	return lines
}

func reportLine(o PayOrder, now time.Time) string {
	name := []rune(o.Name)
	if len(name) > 14 {
		name = append(name[:13], '…')
	}
	parts := []string{o.ID, string(name), fmt.Sprintf("%d 页 %s", o.PageCount(), o.Engine)}
	if o.Total > 0 {
		parts = append(parts, fmt.Sprintf("¥%.2f", float64(o.Total)/100))
	} else if o.CreditPoints > 0 {
		parts = append(parts, fmt.Sprintf("%d 积分", o.CreditPoints))
	}
	if o.Scanned {
		parts = append(parts, "扫描件")
	}
	if o.Delivery == "" {
		parts = append(parts, "已等 "+waited(now.Sub(time.Unix(paidTime(o), 0))))
	}
	return strings.Join(parts, "｜")
}

func waited(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d 小时", int(d.Hours()))
}

func hasScan(orders []PayOrder) bool {
	for _, o := range orders {
		if o.Scanned {
			return true
		}
	}
	return false
}
