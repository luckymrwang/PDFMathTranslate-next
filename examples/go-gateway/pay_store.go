package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type PayConfig struct {
	Enabled                                   bool
	OfferID, AppKey, NotifyToken, AESKey, Dir string
	StandardProduct, EnhancedProduct          string
	StandardPrice, EnhancedPrice              int
}

func loadPayConfig() (*PayConfig, error) {
	c := &PayConfig{Enabled: os.Getenv("VIRTUALPAY_ENABLED") == "true",
		OfferID: os.Getenv("VIRTUALPAY_OFFER_ID"), AppKey: os.Getenv("VIRTUALPAY_APP_KEY"),
		NotifyToken: os.Getenv("VIRTUALPAY_NOTIFY_TOKEN"), AESKey: os.Getenv("VIRTUALPAY_ENCODING_AES_KEY"),
		Dir:             getenv("VIRTUALPAY_DATA_DIR", "./data/virtualpay"),
		StandardProduct: os.Getenv("VIRTUALPAY_STANDARD_PRODUCT_ID"),
		EnhancedProduct: os.Getenv("VIRTUALPAY_ENHANCED_PRODUCT_ID")}
	c.StandardPrice, _ = strconv.Atoi(os.Getenv("VIRTUALPAY_STANDARD_PRICE_FEN"))
	c.EnhancedPrice, _ = strconv.Atoi(os.Getenv("VIRTUALPAY_ENHANCED_PRICE_FEN"))
	if !c.Enabled {
		return c, nil
	}
	if c.OfferID == "" || c.AppKey == "" || c.NotifyToken == "" || c.StandardProduct == "" || c.EnhancedProduct == "" {
		return nil, errors.New("virtual payment enabled but required configuration is missing")
	}
	if c.StandardPrice <= 0 || c.EnhancedPrice <= 0 || c.StandardPrice > 100000 || c.EnhancedPrice > 100000 {
		return nil, errors.New("virtual payment product prices must be valid integer fen")
	}
	if _, err := decodeAESKey(c.AESKey); err != nil {
		return nil, err
	}
	return c, nil
}

// Internal disk record. Only orderView may be returned to a client.
type PayOrder struct {
	ID, OpenID, ProductID                                               string
	Quantity, UnitPrice, Total                                          int
	Engine, LangIn, LangOut, FilePath, FileHash, Name, Attach, SignData string
	State, WxOrderID, TaskID                                            string
	Provided                                                            bool
	CreatedAt, LastQuery                                                int64
	// Delivery is "" while translating, "archived" once results are durable, "failed" when retries are exhausted.
	Delivery           string
	Attempts           int
	TaskHistory        []string
	MonoFile, DualFile string
	MonoKey, DualKey   string
}

type OrderStore struct {
	mu     sync.Mutex
	dir    string
	orders map[string]PayOrder
}

func openOrderStore(dir string) (*OrderStore, error) {
	for _, path := range []string{dir, filepath.Join(dir, "orders"), filepath.Join(dir, "uploads"), filepath.Join(dir, "results")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	store := &OrderStore{dir: dir, orders: make(map[string]PayOrder)}
	files, err := filepath.Glob(filepath.Join(dir, "orders", "*.json"))
	if err != nil {
		return nil, err
	}
	wxIDs := make(map[string]string)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var order PayOrder
		if err := json.Unmarshal(data, &order); err != nil {
			return nil, fmt.Errorf("invalid order file: %s", filepath.Base(path))
		}
		if order.ID == "" || order.ID+".json" != filepath.Base(path) {
			return nil, errors.New("invalid order identity")
		}
		if order.WxOrderID != "" {
			if previous, ok := wxIDs[order.WxOrderID]; ok && previous != order.ID {
				return nil, errors.New("duplicate platform order id")
			}
			wxIDs[order.WxOrderID] = order.ID
		}
		store.orders[order.ID] = order
	}
	return store, nil
}

// One gateway process owns this directory; never share it across instances.
func (s *OrderStore) saveLocked(order PayOrder) error {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "orders"), ".order-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(tmp).Encode(order); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(s.dir, "orders", order.ID+".json")); err != nil {
		return err
	}
	s.orders[order.ID] = order
	if dir, err := os.Open(filepath.Join(s.dir, "orders")); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *OrderStore) add(order PayOrder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orders[order.ID]; ok {
		return errors.New("duplicate order")
	}
	return s.saveLocked(order)
}

func (s *OrderStore) get(id string) (PayOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[id]
	return order, ok
}

func (s *OrderStore) all() []PayOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PayOrder, 0, len(s.orders))
	for _, order := range s.orders {
		out = append(out, order)
	}
	return out
}

// Bound bursts of new orders instead of permanently locking out a user who
// has old unfinished test orders. Historical orders stay available for proof.
func (s *OrderStore) recentOrderCount(owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-time.Minute).Unix()
	count := 0
	for _, order := range s.orders {
		if order.OpenID == owner && order.CreatedAt >= cutoff {
			count++
		}
	}
	return count
}

func (s *OrderStore) update(id string, change func(*PayOrder) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[id]
	if !ok {
		return errors.New("order not found")
	}
	if err := change(&order); err != nil {
		return err
	}
	if order.WxOrderID != "" {
		for otherID, other := range s.orders {
			if otherID != id && other.WxOrderID == order.WxOrderID {
				return errors.New("platform order already used")
			}
		}
	}
	return s.saveLocked(order)
}

func newTradeNo() (string, error) {
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "T" + hex.EncodeToString(raw), nil
}

type paySessions struct {
	mu   sync.Mutex
	keys map[string]paySession
}
type paySession struct {
	Key     string
	Expires time.Time
}

func (s *paySessions) set(token, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for old, session := range s.keys {
		if time.Now().After(session.Expires) {
			delete(s.keys, old)
		}
	}
	s.keys[token] = paySession{Key: key, Expires: time.Now().Add(15 * time.Minute)}
}
func (s *paySessions) get(token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.keys[token]
	if !ok || time.Now().After(session.Expires) {
		return ""
	}
	return session.Key
}
