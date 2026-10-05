package main

import (
	"fmt"
	"os"
	"time"
)

// Config holds runtime configuration, loaded entirely from environment
// variables. Secrets are never hardcoded.
type Config struct {
	AppID     string        // WeChat mini-program AppID
	AppSecret string        // WeChat mini-program AppSecret (secret!)
	JWTSecret []byte        // HMAC key for signing session tokens (secret!)
	Pdf2zhURL string        // base URL of the Python translation API
	Addr      string        // listen address, e.g. ":8080"
	TokenTTL  time.Duration // session token lifetime
}

// Default AppID for this project; override with WECHAT_APPID if needed.
const defaultAppID = "wx760bf26760f46645"

// LoadConfig reads configuration from the environment and validates secrets.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		AppID:     getenv("WECHAT_APPID", defaultAppID),
		AppSecret: os.Getenv("WECHAT_APPSECRET"),
		JWTSecret: []byte(os.Getenv("JWT_SECRET")),
		Pdf2zhURL: getenv("PDF2ZH_API_URL", "http://127.0.0.1:11008"),
		Addr:      getenv("GATEWAY_ADDR", ":8080"),
		TokenTTL:  7 * 24 * time.Hour,
	}

	if cfg.AppSecret == "" {
		return nil, fmt.Errorf("WECHAT_APPSECRET is required (set it via environment, never in code)")
	}
	if len(cfg.JWTSecret) < 16 {
		return nil, fmt.Errorf("JWT_SECRET is required and must be at least 16 bytes")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
