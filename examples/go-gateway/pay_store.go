package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

type PayConfig struct {
	Enabled                                   bool
	TrialEnabled                              bool
	OfferID, AppKey, NotifyToken, AESKey, Dir string
	StandardProduct, EnhancedProduct          string
	StandardPrice, EnhancedPrice              int
	// Engines priced separately; others use the standard or enhanced product.
	EngineProducts map[string]enginePrice
	// iOS (Apple) rejects orders under iosMinFen. With an iOS unit product configured,
	// iOS orders are rounded up to whole units of IOSUnit fen and bought as that product.
	IOSProduct string
	IOSUnit    int
}

// iosMinFen is Apple payment's minimum order amount.
const iosMinFen = 100

type enginePrice struct {
	Product string
	Price   int
}

// engineProductEnv maps an engine to its optional VIRTUALPAY_<PREFIX>_PRODUCT_ID / _PRICE_FEN pair.
var engineProductEnv = map[string]string{"Google": "GOOGLE", "GPT-6": "GPT"}

func loadPayConfig() (*PayConfig, error) {
	c := &PayConfig{Enabled: os.Getenv("VIRTUALPAY_ENABLED") == "true",
		TrialEnabled: os.Getenv("TRIAL_ENABLED") == "true",
		OfferID:      os.Getenv("VIRTUALPAY_OFFER_ID"), AppKey: os.Getenv("VIRTUALPAY_APP_KEY"),
		NotifyToken: os.Getenv("VIRTUALPAY_NOTIFY_TOKEN"), AESKey: os.Getenv("VIRTUALPAY_ENCODING_AES_KEY"),
		Dir:             getenv("VIRTUALPAY_DATA_DIR", "./data/virtualpay"),
		StandardProduct: os.Getenv("VIRTUALPAY_STANDARD_PRODUCT_ID"),
		EnhancedProduct: os.Getenv("VIRTUALPAY_ENHANCED_PRODUCT_ID"),
		EngineProducts:  map[string]enginePrice{}}
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
	for engine, prefix := range engineProductEnv {
		product := os.Getenv("VIRTUALPAY_" + prefix + "_PRODUCT_ID")
		rawPrice := os.Getenv("VIRTUALPAY_" + prefix + "_PRICE_FEN")
		if product == "" && rawPrice == "" {
			continue
		}
		price, err := strconv.Atoi(rawPrice)
		if product == "" || err != nil || price <= 0 || price > 100000 {
			return nil, fmt.Errorf("VIRTUALPAY_%s_PRODUCT_ID and VIRTUALPAY_%s_PRICE_FEN must be set together with a valid fen price", prefix, prefix)
		}
		c.EngineProducts[engine] = enginePrice{product, price}
	}
	if _, err := decodeAESKey(c.AESKey); err != nil {
		return nil, err
	}
	c.IOSProduct = os.Getenv("VIRTUALPAY_IOS_PRODUCT_ID")
	if rawUnit := os.Getenv("VIRTUALPAY_IOS_PRICE_FEN"); c.IOSProduct != "" || rawUnit != "" {
		unit, err := strconv.Atoi(rawUnit)
		if c.IOSProduct == "" || err != nil || unit <= 0 || unit > iosMinFen {
			return nil, errors.New("VIRTUALPAY_IOS_PRODUCT_ID and VIRTUALPAY_IOS_PRICE_FEN must be set together (unit price in fen, at most 100)")
		}
		c.IOSUnit = unit
	}
	return c, nil
}

// productFor returns the virtual-payment product and per-page price for an engine.
func (c *PayConfig) productFor(engine string) (string, int) {
	if p, ok := c.EngineProducts[engine]; ok {
		return p.Product, p.Price
	}
	if standardEngine(engine) {
		return c.StandardProduct, c.StandardPrice
	}
	return c.EnhancedProduct, c.EnhancedPrice
}

// orderAmounts returns the product, per-unit price, units bought and total (fen)
// for an order. Without an iOS unit product the amount is simply pages x price.
func (c *PayConfig) orderAmounts(engine string, pages int, ios bool) (product string, unit, quantity, total int) {
	product, unit = c.productFor(engine)
	if !ios || c.IOSProduct == "" {
		return product, unit, pages, pages * unit
	}
	ceil := func(amount int) int { return (amount + c.IOSUnit - 1) / c.IOSUnit }
	units := ceil(pages * unit)
	if minUnits := ceil(iosMinFen); units < minUnits {
		units = minUnits
	}
	return c.IOSProduct, c.IOSUnit, units, units * c.IOSUnit
}

// Internal disk record. Only orderView may be returned to a client.
type PayOrder struct {
	ID, OpenID, ProductID      string
	Quantity, UnitPrice, Total int
	// Pages is the document page count; Quantity is the number of units bought and
	// differs from it for iOS rounded orders. Zero on orders created before Pages existed.
	Pages                                                               int
	Engine, LangIn, LangOut, FilePath, FileHash, Name, Attach, SignData string
	State, WxOrderID, TaskID                                            string
	Provided                                                            bool
	// DocumentPages preserves the full source length for a trial-to-paid upgrade.
	DocumentPages int
	// UpgradeFrom is the trial order whose translated pages were credited; TrialCredit
	// is how many pages were left out of Quantity. Pages still covers the whole document.
	UpgradeFrom string
	TrialCredit int
	// ReferredBy is the inviter of a trial's new user. CreditPages are referral pages
	// spent on this order, given back once (CreditReturned) if it closes unpaid or is refunded.
	ReferredBy     string
	CreditPages    int
	CreditReturned bool
	// PaymentClosed records a platform-confirmed terminal unpaid order.
	PaymentClosed bool
	// Trial orders are free and never touch WeChat payment.
	Trial                bool
	CreatedAt, LastQuery int64
	// Delivery is "" while translating, "archived" once results are durable, "failed" when retries are exhausted.
	Delivery           string
	FailReason         string
	Attempts           int
	TaskHistory        []string
	MonoFile, DualFile string
	MonoKey, DualKey   string
}

// PageCount returns the document's page count, falling back to Quantity for older orders.
func (o PayOrder) PageCount() int {
	if o.Pages > 0 {
		return o.Pages
	}
	return o.Quantity
}

type OrderStore struct {
	mu     sync.Mutex
	dir    string
	orders map[string]PayOrder
	db     *sql.DB
}

func openOrderStore(dir string) (*OrderStore, error) {
	db, err := openDatabase(dir)
	if err != nil {
		return nil, err
	}
	store := &OrderStore{dir: dir, db: db, orders: make(map[string]PayOrder)}
	rows, err := db.Query("SELECT record FROM orders")
	if err != nil {
		db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			db.Close()
			return nil, err
		}
		var order PayOrder
		if err := json.Unmarshal(data, &order); err != nil {
			db.Close()
			return nil, err
		}
		store.orders[order.ID] = order
	}
	if err := rows.Err(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// The mutex serializes callbacks; publish the in-memory snapshot only after commit.
func (s *OrderStore) saveLocked(order PayOrder) error {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := settleOrderTx(tx, &order); err != nil {
		return err
	}
	if err := writeOrder(tx, order); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.orders[order.ID] = order
	return nil
}

func (s *OrderStore) add(order PayOrder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orders[order.ID]; ok {
		return errors.New("duplicate order")
	}
	if order.UpgradeFrom != "" && order.TrialCredit > 0 {
		for _, existing := range s.orders {
			if existing.UpgradeFrom == order.UpgradeFrom && existing.TrialCredit > 0 && !existing.creditReleased() {
				return errors.New("trial credit already reserved")
			}
		}
	}
	return s.saveLocked(order)
}

// addPurchase reserves referral pages and inserts the priced order atomically.
func (s *OrderStore) addPurchase(order *PayOrder, billed int, price func(*PayOrder, int) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.orders[order.ID]; exists {
		return errors.New("duplicate order")
	}
	if order.UpgradeFrom != "" && order.TrialCredit > 0 {
		for _, existing := range s.orders {
			if existing.UpgradeFrom == order.UpgradeFrom && existing.TrialCredit > 0 && !existing.creditReleased() {
				return errors.New("trial credit already reserved")
			}
		}
	}
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	candidate := *order
	if standardEngine(candidate.Engine) {
		var balance int
		err := tx.QueryRow("SELECT credit FROM referral_users WHERE openid=?", candidate.OpenID).Scan(&balance)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		candidate.CreditPages = min(balance, billed)
		if candidate.CreditPages > 0 {
			if _, err := tx.Exec("UPDATE referral_users SET credit=credit-? WHERE openid=?", candidate.CreditPages, candidate.OpenID); err != nil {
				return err
			}
		}
	}
	if err := price(&candidate, billed-candidate.CreditPages); err != nil {
		return err
	}
	if err := writeOrder(tx, candidate); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*order = candidate
	s.orders[order.ID] = candidate
	return nil
}

// creditReleased excludes local cancellation: those orders can still be paid late.
func (o PayOrder) creditReleased() bool {
	return o.State == "refunded" || (o.State == "closed" && o.PaymentClosed)
}

// trialUnlimited lifts the one-trial-per-user limit for testing (TRIAL_UNLIMITED=true).
func trialUnlimited() bool { return os.Getenv("TRIAL_UNLIMITED") == "true" }

func (s *OrderStore) hasTrial(owner string) bool {
	if trialUnlimited() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, order := range s.orders {
		if order.Trial && order.OpenID == owner {
			return true
		}
	}
	return false
}

// addTrial atomically enforces one free trial per user.
func (s *OrderStore) addTrial(order PayOrder) error {
	return s.addInvitedTrial(order, "")
}

func (s *OrderStore) addInvitedTrial(order PayOrder, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.orders {
		if existing.Trial && existing.OpenID == order.OpenID && !trialUnlimited() {
			return errTrialUsed
		}
	}
	if _, ok := s.orders[order.ID]; ok {
		return errors.New("duplicate order")
	}
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if code != "" {
		newUser := true
		for _, existing := range s.orders {
			if existing.OpenID == order.OpenID {
				newUser = false
				break
			}
		}
		if newUser {
			err := tx.QueryRow("SELECT openid FROM referral_users WHERE code=? AND openid<>?", code, order.OpenID).Scan(&order.ReferredBy)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	if err := writeOrder(tx, order); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.orders[order.ID] = order
	return nil
}

var errTrialUsed = errors.New("trial already used")

// standardEngine reports the machine-translation engines; referral pages only cover these.
func standardEngine(engine string) bool { return engine == "Google" || engine == "Bing" }

// hasOrders reports whether the user has ever created an order; only new users can be invited.
func (s *OrderStore) hasOrders(owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, order := range s.orders {
		if order.OpenID == owner {
			return true
		}
	}
	return false
}

// trialCredit returns how many already-translated trial pages an upgrade of the same
// file may leave unpaid. Each trial is credited once: pending and locally cancelled upgrades keep their reservation.
func (s *OrderStore) trialCredit(trialID, owner, fileHash string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	trial, ok := s.orders[trialID]
	if !ok || !trial.Trial || trial.OpenID != owner || trial.FileHash == "" || trial.FileHash != fileHash {
		return 0, false
	}
	for _, order := range s.orders {
		if order.UpgradeFrom == trialID && order.TrialCredit > 0 && !order.creditReleased() {
			return 0, false
		}
	}
	return trial.PageCount(), true
}

func (s *OrderStore) get(id string) (PayOrder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[id]
	order.TaskHistory = append([]string(nil), order.TaskHistory...)
	return order, ok
}

func (s *OrderStore) all() []PayOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PayOrder, 0, len(s.orders))
	for _, order := range s.orders {
		order.TaskHistory = append([]string(nil), order.TaskHistory...)
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
	order.TaskHistory = append([]string(nil), order.TaskHistory...)
	if err := change(&order); err != nil {
		return err
	}
	// Also protect orders written by older versions before reservations were enforced.
	if order.TrialCredit > 0 && order.UpgradeFrom != "" && (order.State == "paid" || order.State == "submitted") {
		for otherID, other := range s.orders {
			if otherID != id && other.UpgradeFrom == order.UpgradeFrom && other.TrialCredit > 0 &&
				(other.State == "paid" || other.State == "submitted") {
				return errors.New("trial credit already consumed")
			}
		}
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
