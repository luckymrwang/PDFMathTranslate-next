package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

const maxPDFBytes = 30 << 20

// logOversize emits one greppable line per rejected upload: grep "oversize_pdf" go-gateway.log
func logOversize(r *http.Request, source string, size int64) {
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	sum := sha256.Sum256([]byte(openid))
	log.Printf("oversize_pdf source=%s size_mb=%.1f user=%s", source, float64(size)/(1<<20), hex.EncodeToString(sum[:4]))
}

func isBodyTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// handleOversize records files the mini-program refused locally, which never reach an upload route.
func (s *Server) handleOversize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Size int64 `json:"size"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 256)).Decode(&input) != nil || input.Size <= maxPDFBytes {
		writeError(w, 400, "invalid size")
		return
	}
	logOversize(r, "client", input.Size)
	w.WriteHeader(http.StatusNoContent)
}
