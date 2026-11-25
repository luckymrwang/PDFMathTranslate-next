package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTranslationAPI serves /health with a scripted "azure" state.
type fakeTranslationAPI struct {
	mu     sync.Mutex
	azure  string
	server *httptest.Server
}

func newFakeTranslationAPI(t *testing.T, azure string) *fakeTranslationAPI {
	t.Helper()
	f := &fakeTranslationAPI{azure: azure}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"status":"ok","active_tasks":0,"azure":%s}`, f.azure)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTranslationAPI) set(azure string) {
	f.mu.Lock()
	f.azure = azure
	f.mu.Unlock()
}

func azureFallbackJSON(since time.Time, status int) string {
	return fmt.Sprintf(`{"state":"fallback","since":%d,"last_block":%d,"status":%d,"reason":"quota"}`,
		since.Unix(), since.Unix(), status)
}

func TestAzureFallbackIsAlertedOnceAndRecoveryOnce(t *testing.T) {
	sink := newAlertSink(t)
	api := newFakeTranslationAPI(t, `{"state":"ok"}`)
	s := &Server{pdf2zh: NewPdf2zhClient(api.server.URL)}
	ctx := context.Background()
	now := time.Now()

	s.watchAzure(ctx, now) // healthy: nothing to say
	time.Sleep(50 * time.Millisecond)
	if n := sink.count(""); n != 0 {
		t.Fatalf("alerts while healthy: %q", sink.titles)
	}

	api.set(azureFallbackJSON(now, 403))
	for i := 0; i < 5; i++ { // polled every minute for as long as the outage lasts
		s.watchAzure(ctx, now.Add(time.Duration(i)*time.Minute))
	}
	sink.wait(t, "免费额度已用完")
	time.Sleep(100 * time.Millisecond)
	if n := sink.count(""); n != 1 {
		t.Fatalf("one alert per outage, got %d: %q", n, sink.titles)
	}

	api.set(`{"state":"ok"}`)
	s.watchAzure(ctx, now.Add(95*time.Minute))
	s.watchAzure(ctx, now.Add(96*time.Minute))
	sink.wait(t, "已恢复使用 Azure")
	time.Sleep(100 * time.Millisecond)
	if n := sink.count(""); n != 2 {
		t.Fatalf("one recovery alert, got %d total: %q", n, sink.titles)
	}
}

func TestAzureAlertNamesTheCause(t *testing.T) {
	for status, want := range map[int]string{403: "免费额度已用完", 401: "密钥被拒绝", 500: "不可用（500）"} {
		title, body := azureFallbackMessage(azureHealth{State: "fallback", Since: time.Now().Unix(), Status: status, Reason: "r"})
		if !strings.Contains(title, want) {
			t.Errorf("status %d: title %q lacks %q", status, title, want)
		}
		if !strings.Contains(body, "30 分钟") {
			t.Errorf("status %d: body does not say when Azure is retried: %q", status, body)
		}
	}
}

func TestAzureOldOutageIsNotReannouncedAfterARestart(t *testing.T) {
	sink := newAlertSink(t)
	now := time.Now()
	api := newFakeTranslationAPI(t, azureFallbackJSON(now.Add(-5*time.Hour), 403))
	s := &Server{pdf2zh: NewPdf2zhClient(api.server.URL)}
	s.watchAzure(context.Background(), now)
	time.Sleep(100 * time.Millisecond)
	if n := sink.count(""); n != 0 {
		t.Fatalf("a five-hour-old outage was announced again: %q", sink.titles)
	}

	// ...but its end is still reported, and a fresh outage after a restart is announced.
	api.set(`{"state":"ok"}`)
	s.watchAzure(context.Background(), now.Add(time.Minute))
	sink.wait(t, "已恢复使用 Azure")

	fresh := newFakeTranslationAPI(t, azureFallbackJSON(now.Add(-5*time.Minute), 401))
	s2 := &Server{pdf2zh: NewPdf2zhClient(fresh.server.URL)}
	s2.watchAzure(context.Background(), now)
	sink.wait(t, "密钥被拒绝")
}

func TestAzureHealthErrorsAreQuiet(t *testing.T) {
	sink := newAlertSink(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)
	s := &Server{pdf2zh: NewPdf2zhClient(down.URL)}
	s.watchAzure(context.Background(), time.Now())
	s.watchAzure(context.Background(), time.Now())
	time.Sleep(50 * time.Millisecond)
	if n := sink.count(""); n != 0 {
		t.Fatalf("an unreachable API is not an Azure outage: %q", sink.titles)
	}
}
