package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Server holds the gateway dependencies.
type Server struct {
	prepared preparedFiles
	cfg      *Config
	wx       *WeChatClient
	pdf2zh   *Pdf2zhClient
	tasks    *taskRegistry
	storage  *S3Client // nil when object storage is not configured
	pay      *PayService
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	srv := &Server{
		cfg:    cfg,
		wx:     NewWeChatClient(cfg.AppID, cfg.AppSecret),
		pdf2zh: NewPdf2zhClient(cfg.Pdf2zhURL),
		tasks:  newTaskRegistry(),
	}
	payConfig, err := loadPayConfig()
	if err != nil {
		log.Fatalf("payment config error: %v", err)
	}
	if payConfig.Enabled {
		store, err := openOrderStore(payConfig.Dir)
		if err != nil {
			log.Fatalf("payment storage error: %v", err)
		}
		if err := cleanupPreparedFiles(filepath.Join(store.dir, "uploads")); err != nil {
			log.Fatalf("preparation cleanup error: %v", err)
		}
		srv.pay = &PayService{cfg: payConfig, store: store, client: newXPayClient(cfg, payConfig),
			sessions: &paySessions{keys: make(map[string]paySession)}}
		for _, order := range store.all() {
			if order.TaskID != "" {
				srv.tasks.set(order.TaskID, order.OpenID)
			}
		}
		go srv.runPayReconciliation(context.Background())
		log.Printf("virtual payment enabled (production environment, durable order store)")
	}
	if s3cfg, ok := LoadS3Config(); ok {
		srv.storage = NewS3Client(s3cfg)
		log.Printf("object storage enabled: bucket=%s endpoint=%s", s3cfg.Bucket, s3cfg.Endpoint)
	} else {
		log.Printf("object storage not configured; result archiving disabled")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/login", srv.handleLogin)
	mux.Handle("/api/me", srv.authRequired(http.HandlerFunc(srv.handleMe)))
	mux.Handle("/api/engines", srv.authRequired(http.HandlerFunc(srv.handleEngines)))
	mux.Handle("/api/engines/check", srv.authRequired(http.HandlerFunc(srv.handleEngineCheck)))
	mux.Handle("/pay/prepare", srv.authRequired(http.HandlerFunc(srv.handlePreparePDF)))
	mux.Handle("/pay/order/prepared", srv.authRequired(http.HandlerFunc(srv.handlePreparedOrder)))
	mux.Handle("/api/translate", srv.authRequired(http.HandlerFunc(srv.handleTranslateSubmit)))
	mux.Handle("/api/translate/", srv.authRequired(http.HandlerFunc(srv.handleTranslateTask)))
	mux.Handle("/pay/config", srv.authRequired(http.HandlerFunc(srv.handlePayConfig)))
	mux.Handle("/pay/order", srv.authRequired(http.HandlerFunc(srv.handlePayOrder)))
	mux.Handle("/pay/orders", srv.authRequired(http.HandlerFunc(srv.handlePayOrders)))
	mux.Handle("/pay/orders/", srv.authRequired(http.HandlerFunc(srv.handlePayOrders)))
	mux.Handle("/pay/query", srv.authRequired(http.HandlerFunc(srv.handlePayQuery)))
	mux.HandleFunc("/pay/notify", srv.handlePayNotify)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("gateway listening on %s (appid=%s, pdf2zh=%s)", cfg.Addr, cfg.AppID, cfg.Pdf2zhURL)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// loginRequest is the body of POST /api/login.
type loginRequest struct {
	Code string `json:"code"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"` // seconds
	OpenID    string `json:"openid"`
}

// handleLogin exchanges a wx.login code for a session token.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	session, err := s.wx.Code2Session(ctx, req.Code)
	if err != nil {
		log.Printf("code2session failed: %v", err)
		writeError(w, http.StatusUnauthorized, "wechat login failed")
		return
	}

	token, err := IssueToken(s.cfg.JWTSecret, session.OpenID, s.cfg.TokenTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	if s.payEnabled() {
		if session.SessionKey == "" {
			writeError(w, 502, "wechat returned no session key")
			return
		}
		s.pay.sessions.set(token, session.SessionKey)
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		ExpiresIn: int(s.cfg.TokenTTL.Seconds()),
		OpenID:    session.OpenID,
	})
}

// handleMe returns the authenticated user's openid (example protected route).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	writeJSON(w, http.StatusOK, map[string]string{"openid": openid})
}

// --- auth middleware -----------------------------------------------------------

type ctxKey string

const ctxKeyOpenID ctxKey = "openid"

// authRequired validates the Bearer session token and injects openid into ctx.
func (s *Server) authRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimSpace(auth[len(prefix):])
		openid, err := VerifyToken(s.cfg.JWTSecret, token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyOpenID, openid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// --- helpers -------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// copyHeader copies the named headers from src to dst, skipping empty values.
func copyHeader(dst, src http.Header, names ...string) {
	for _, n := range names {
		if v := src.Get(n); v != "" {
			dst.Set(n, v)
		}
	}
}

// copyBody streams a response body to the client.
func copyBody(w http.ResponseWriter, body io.Reader) (int64, error) {
	return io.Copy(w, body)
}
