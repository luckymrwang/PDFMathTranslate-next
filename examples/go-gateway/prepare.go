package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type preparedPDF struct {
	Owner, Path, Name, Hash string
	Pages                   int
	Expires                 time.Time
}

type preparedFiles struct {
	sync.Mutex
	files map[string]preparedPDF
}

// Credentials are process-local. Reclaim only our preparation files on restart;
// independent .order-* links remain owned by the durable order store.
func cleanupPreparedFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".prepare-") && strings.HasSuffix(entry.Name(), ".pdf") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) checkEngine(ctx context.Context, engine string) error {
	settings, ok := availableEngines()[engine]
	if !ok {
		return errors.New("engine not configured")
	}
	body, _ := json.Marshal(map[string]any{"translate_engine_settings": settings})
	ctx, cancel := context.WithTimeout(ctx, defaultSubmitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.pdf2zh.BaseURL+"/v1/engine/check", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.pdf2zh.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("engine unavailable")
	}
	return nil
}

func (s *Server) handleEngineCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var input struct {
		Engine string `json:"engine"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeError(w, 400, "invalid settings")
		return
	}
	if s.checkEngine(r.Context(), input.Engine) != nil {
		writeError(w, 503, "翻译引擎暂不可用")
		return
	}
	writeJSON(w, 200, map[string]bool{"ready": true})
}

func (s *Server) handlePreparePDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	owner, _ := r.Context().Value(ctxKeyOpenID).(string)
	r.Body = http.MaxBytesReader(w, r.Body, 51<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, 400, "PDF 最大 50 MB")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, "请选择 PDF 文件")
		return
	}
	defer file.Close()
	// wx.uploadFile uses a temporary basename; preserve the original display name separately.
	displayName := filepath.Base(header.Filename)
	if raw := r.FormValue("data"); raw != "" {
		var metadata struct {
			FileName string `json:"file_name"`
		}
		if len(raw) > 4096 || json.Unmarshal([]byte(raw), &metadata) != nil {
			writeError(w, 400, "invalid file metadata")
			return
		}
		if metadata.FileName != "" {
			displayName = filepath.Base(strings.ReplaceAll(metadata.FileName, "\\", "/"))
			if len(displayName) > 1024 || strings.ContainsAny(displayName, "\r\n\x00") || !strings.EqualFold(filepath.Ext(displayName), ".pdf") {
				writeError(w, 400, "invalid PDF name")
				return
			}
		}
	}
	if header.Size <= 0 || header.Size > 50<<20 || !strings.EqualFold(filepath.Ext(header.Filename), ".pdf") {
		writeError(w, 400, "请选择不超过 50 MB 的 PDF")
		return
	}
	// Reserve a slot before disk/network work to bound concurrent uploads per user.
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		writeError(w, 500, "无法准备文件")
		return
	}
	id := hex.EncodeToString(random[:])
	s.prepared.Lock()
	if s.prepared.files == nil {
		s.prepared.files = make(map[string]preparedPDF)
	}
	count := 0
	for _, f := range s.prepared.files {
		if f.Owner == owner {
			count++
		}
	}
	if count >= 5 || len(s.prepared.files) >= 100 {
		s.prepared.Unlock()
		writeError(w, 429, "准备中的文件过多，请稍后再试")
		return
	}
	s.prepared.files[id] = preparedPDF{Owner: owner}
	s.prepared.Unlock()
	keep := false
	defer func() {
		if !keep {
			s.prepared.Lock()
			delete(s.prepared.files, id)
			s.prepared.Unlock()
		}
	}()
	staged, err := os.CreateTemp(filepath.Join(s.pay.store.dir, "uploads"), ".prepare-*.pdf")
	if err != nil {
		writeError(w, 500, "无法保存文件")
		return
	}
	defer func() {
		staged.Close()
		if !keep {
			os.Remove(staged.Name())
		}
	}()
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(staged, hash), file)
	if err == nil {
		err = staged.Sync()
	}
	if err == nil {
		err = staged.Close()
	}
	if err != nil {
		writeError(w, 500, "无法保存文件")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultSubmitTimeout)
	defer cancel()
	// No engine settings: inspecting a file only reads its page count.
	raw, err := s.uploadFile(ctx, "/v1/pdf/inspect", staged.Name(), header.Filename, nil, "")
	var result struct {
		Pages int `json:"page_count"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Pages < 1 || result.Pages > 1000 {
		writeError(w, 422, "无法读取 PDF 页数")
		return
	}
	expires := time.Now().Add(30 * time.Minute)
	s.prepared.Lock()
	s.prepared.files[id] = preparedPDF{owner, staged.Name(), displayName,
		hex.EncodeToString(hash.Sum(nil)), result.Pages, expires}
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
	writeJSON(w, 200, map[string]any{"file_token": id, "page_count": result.Pages, "expires_at": expires.Unix()})
}

// A distinct hard link lets each order retain its file after the preparation expires.
func (s *Server) reusePrepared(id, owner string) (preparedPDF, error) {
	s.prepared.Lock()
	defer s.prepared.Unlock()
	f, ok := s.prepared.files[id]
	if !ok || f.Owner != owner || f.Path == "" || !time.Now().Before(f.Expires) {
		return preparedPDF{}, errors.New("file expired")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return preparedPDF{}, err
	}
	path := filepath.Join(s.pay.store.dir, "uploads", ".order-"+hex.EncodeToString(random[:])+".pdf")
	if err := os.Link(f.Path, path); err != nil {
		return preparedPDF{}, err
	}
	f.Path = path
	return f, nil
}

func (s *Server) handlePreparedOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.payEnabled() {
		writeError(w, 503, "虚拟支付尚未配置")
		return
	}
	session := s.pay.sessions.get(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if session == "" {
		writeError(w, 401, "请重新登录后支付")
		return
	}
	owner, _ := r.Context().Value(ctxKeyOpenID).(string)
	var input struct {
		FileToken string `json:"file_token"`
		Engine    string `json:"engine"`
		LangIn    string `json:"lang_in"`
		LangOut   string `json:"lang_out"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		writeError(w, 400, "invalid settings")
		return
	}
	languages := map[string]bool{"zh": true, "en": true, "ja": true, "ko": true, "fr": true, "de": true, "es": true}
	if !languages[input.LangIn] || !languages[input.LangOut] || input.LangIn == input.LangOut {
		writeError(w, 400, "请选择不同的源语言与目标语言")
		return
	}
	if _, ok := availableEngines()[input.Engine]; !ok {
		writeError(w, 400, "翻译引擎未配置")
		return
	}
	file, err := s.reusePrepared(input.FileToken, owner)
	if err != nil {
		writeError(w, 410, "文件凭证已失效，请重新选择文件")
		return
	}
	saved := false
	defer func() {
		if !saved {
			os.Remove(file.Path)
		}
	}()
	if s.checkEngine(r.Context(), input.Engine) != nil {
		writeError(w, 503, "翻译引擎暂不可用，请稍后重试")
		return
	}
	// Serialize quota check and order creation across prepared requests.
	s.pay.startMu.Lock()
	defer s.pay.startMu.Unlock()
	if s.pay.store.recentOrderCount(owner) >= 10 {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "操作过于频繁，请一分钟后重试")
		return
	}
	product, price := s.pay.cfg.EnhancedProduct, s.pay.cfg.EnhancedPrice
	if input.Engine == "Google" || input.Engine == "Bing" {
		product, price = s.pay.cfg.StandardProduct, s.pay.cfg.StandardPrice
	}
	if file.Pages*price > 10000000 {
		writeError(w, 400, "订单金额超过限额")
		return
	}
	id, err := newTradeNo()
	if err != nil {
		writeError(w, 500, "无法创建订单")
		return
	}
	order := PayOrder{ID: id, OpenID: owner, ProductID: product, UnitPrice: price,
		Quantity: file.Pages, Total: file.Pages * price, Engine: input.Engine,
		LangIn: input.LangIn, LangOut: input.LangOut, FilePath: file.Path,
		FileHash: file.Hash, Name: file.Name, State: "pending", CreatedAt: time.Now().Unix(), Attach: id}
	signData, _ := json.Marshal(map[string]any{"offerId": s.pay.cfg.OfferID,
		"buyQuantity": order.Quantity, "env": 0, "currencyType": "CNY",
		"productId": order.ProductID, "goodsPrice": order.UnitPrice, "outTradeNo": id, "attach": id})
	order.SignData = string(signData)
	if err := s.pay.store.add(order); err != nil {
		writeError(w, 500, "订单保存失败")
		return
	}
	saved = true
	writeJSON(w, 200, map[string]any{"order": orderView(order), "payData": paymentData(order, s.pay.cfg.AppKey, session)})
}
