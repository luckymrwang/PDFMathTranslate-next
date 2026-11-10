package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"time"
)

// LAN mode (LAN_MODE=true): the intranet web edition of the mini-program.
//
//   - Browsers authenticate with LAN_USERNAME and LAN_PASSWORD and use a
//     device id to associate translations with their browser.
//   - srv.pay stays nil, so ordersEnabled() is false and the existing
//     /api/translate route runs translations directly: no payment, no points.
//   - The static web UI under web/ is embedded into the binary and served on "/".
//
// The public mini-program deployment never sets LAN_MODE and is unaffected.

//go:embed web
var webFS embed.FS

var lanDeviceID = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

func (s *Server) lanTokenSecret() []byte {
	mac := hmac.New(sha256.New, s.cfg.JWTSecret)
	// Length-prefix both credentials to keep their encoding unambiguous.
	_ = json.NewEncoder(mac).Encode([]string{"lan-auth-v1", s.cfg.LanUsername, s.cfg.LanPassword})
	return mac.Sum(nil)
}

func serveLan(srv *Server, cfg *Config) {
	web, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embedded web assets: %v", err)
	}

	store, err := openLanModelStore(cfg.LanDataDir)
	if err != nil {
		log.Fatalf("LAN model configuration: %v", err)
	}
	srv.lanSettings = store
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/login", srv.handleLanLogin)
	mux.Handle("/api/engines", srv.authRequired(http.HandlerFunc(srv.handleLanEngines)))
	mux.Handle("/api/model-config", srv.authRequired(http.HandlerFunc(srv.handleLanModelConfig)))
	mux.Handle("/api/translate", srv.authRequired(http.HandlerFunc(srv.handleTranslateSubmit)))
	mux.Handle("/api/translate/", srv.authRequired(http.HandlerFunc(srv.handleTranslateTask)))
	mux.Handle("/", noCache(http.FileServer(http.FS(web))))

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("gateway (LAN web mode) listening on %s (password=%t, pdf2zh=%s)",
		cfg.Addr, cfg.LanPassword != "", cfg.Pdf2zhURL)
	log.Fatalf("server error: %v", httpServer.ListenAndServe())
}

// noCache keeps the browser from running a stale UI after the image is upgraded.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// handleLanLogin issues a session token for a browser device id.
// GET /api/login reports that credentials are required.
// POST /api/login accepts device_id, username and password.
func (s *Server) handleLanLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]bool{"password_required": true})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	userHash, expectedUser := sha256.Sum256([]byte(req.Username)), sha256.Sum256([]byte(s.cfg.LanUsername))
	passHash, expectedPass := sha256.Sum256([]byte(req.Password)), sha256.Sum256([]byte(s.cfg.LanPassword))
	if subtle.ConstantTimeCompare(userHash[:], expectedUser[:])&subtle.ConstantTimeCompare(passHash[:], expectedPass[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "访问密码错误")
		return
	}
	if req.DeviceID == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create device id")
			return
		}
		req.DeviceID = hex.EncodeToString(buf)
	} else if !lanDeviceID.MatchString(req.DeviceID) {
		writeError(w, http.StatusBadRequest, "invalid device id")
		return
	}
	openid := "lan-auth-" + req.DeviceID
	token, err := IssueToken(s.lanTokenSecret(), openid, s.cfg.TokenTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue token")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresIn: int(s.cfg.TokenTTL.Seconds()), OpenID: openid})
}
