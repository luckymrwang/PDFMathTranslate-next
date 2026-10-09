package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Reuse only server-owned files from terminal orders. No payment/order is created here.
func (s *Server) handleRepreparePDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	var input struct {
		OrderID string `json:"order_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeError(w, 400, "invalid order")
		return
	}
	owner, _ := r.Context().Value(ctxKeyOpenID).(string)
	order, ok := s.pay.store.get(input.OrderID)
	if !ok || order.OpenID != owner {
		writeError(w, 404, "订单不存在")
		return
	}
	if order.State != "closed" && order.State != "refunded" {
		writeError(w, 409, "请先确认原订单状态，避免重复支付")
		return
	}
	if order.Quantity < 1 || order.Quantity > 1000 || order.FileHash == "" {
		writeError(w, 410, "原文件凭证已失效，请重新选择 PDF")
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		writeError(w, 500, "无法准备文件")
		return
	}
	id := hex.EncodeToString(random[:])
	// Reserving before hashing bounds concurrent verification work too.
	s.prepared.Lock()
	if s.prepared.files == nil {
		s.prepared.files = make(map[string]preparedPDF)
	}
	count, oldest := 0, ""
	var expiry time.Time
	for key, f := range s.prepared.files {
		if f.Owner != owner {
			continue
		}
		count++
		if f.Path != "" && (oldest == "" || f.Expires.Before(expiry)) {
			oldest, expiry = key, f.Expires
		}
	}
	if count >= 5 && oldest != "" {
		if err := os.Remove(s.prepared.files[oldest].Path); err == nil || os.IsNotExist(err) {
			delete(s.prepared.files, oldest)
			count--
		}
	}
	if count >= 5 || len(s.prepared.files) >= 100 {
		s.prepared.Unlock()
		writeError(w, 429, "同时准备的文件过多，请稍后重试")
		return
	}
	s.prepared.files[id] = preparedPDF{Owner: owner}
	s.prepared.Unlock()
	linked := filepath.Join(s.pay.store.dir, "uploads", ".prepare-"+id+".pdf")
	keep := false
	defer func() {
		if !keep {
			os.Remove(linked)
			s.prepared.Lock()
			delete(s.prepared.files, id)
			s.prepared.Unlock()
		}
	}()
	// A new hard link isolates preparation expiry from the original order file.
	if os.Link(order.FilePath, linked) != nil {
		writeError(w, 410, "服务器原文件已失效，请重新上传 PDF")
		return
	}
	file, err := os.Open(linked)
	if err != nil {
		writeError(w, 410, "服务器原文件已失效，请重新上传 PDF")
		return
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, (50<<20)+1))
	if err != nil || size < 1 || size > 50<<20 || hex.EncodeToString(hash.Sum(nil)) != order.FileHash {
		writeError(w, 410, "原文件完整性校验失败，请重新上传 PDF")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	expires := time.Now().Add(30 * time.Minute)
	s.prepared.Lock()
	s.prepared.files[id] = preparedPDF{Owner: owner, Path: linked, Name: order.Name, Hash: order.FileHash, Pages: order.Quantity, Expires: expires}
	s.prepared.Unlock()
	keep = true
	time.AfterFunc(30*time.Minute, func() {
		s.prepared.Lock()
		defer s.prepared.Unlock()
		if f, ok := s.prepared.files[id]; ok {
			os.Remove(f.Path)
			delete(s.prepared.files, id)
		}
	})
	writeJSON(w, 200, map[string]any{"file_token": id, "page_count": order.Quantity, "file_name": order.Name, "file_size": size, "expires_at": expires.Unix()})
}
