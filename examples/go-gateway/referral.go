package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// An inviter earns referralRewardPages free standard-translation pages for each
// invited new user whose free trial is delivered. There is no cap: every reward
// needs a real new user's delivered trial, and the pages only cover cheap Bing/Google.
const referralRewardPages = 5

type referralUser struct {
	Code string `json:"code"`
	// Credit is the unspent page balance; pages reserved by open orders are not in it.
	Credit   int             `json:"credit"`
	Rewarded []string        `json:"rewarded"`
	Returned map[string]bool `json:"returned,omitempty"`
}

// ReferralStore keeps invite codes and page credits in one JSON file next to the orders.
type ReferralStore struct {
	mu    sync.Mutex
	path  string
	users map[string]*referralUser
	codes map[string]string
}

func openReferralStore(dir string) (*ReferralStore, error) {
	store := &ReferralStore{path: filepath.Join(dir, "referrals.json"),
		users: make(map[string]*referralUser), codes: make(map[string]string)}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &store.users); err != nil {
		return nil, errors.New("invalid referral file")
	}
	for openid, user := range store.users {
		store.codes[user.Code] = openid
	}
	return store, nil
}

func (s *ReferralStore) saveLocked() error {
	data, err := json.Marshal(s.users)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".referrals-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// userLocked returns the user's record, creating it with a fresh invite code.
func (s *ReferralStore) userLocked(openid string) (*referralUser, error) {
	if user, ok := s.users[openid]; ok {
		return user, nil
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		code := make([]byte, len(raw))
		for i, b := range raw {
			code[i] = alphabet[int(b)%len(alphabet)]
		}
		if _, taken := s.codes[string(code)]; !taken {
			user := &referralUser{Code: string(code)}
			s.users[openid] = user
			s.codes[user.Code] = openid
			return user, nil
		}
	}
}

func (s *ReferralStore) view(openid string) (referralUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.users[openid]
	user, err := s.userLocked(openid)
	if err != nil {
		return referralUser{}, err
	}
	if !existed {
		if err := s.saveLocked(); err != nil {
			return referralUser{}, err
		}
	}
	return *user, nil
}

func (s *ReferralStore) credit(openid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if user, ok := s.users[openid]; ok {
		return user.Credit
	}
	return 0
}

// inviter resolves an invite code; nobody can invite themselves.
func (s *ReferralStore) inviter(code, invitee string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner := s.codes[code]; owner != invitee {
		return owner
	}
	return ""
}

// reward credits the inviter once per invitee.
func (s *ReferralStore) reward(inviter, invitee string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, err := s.userLocked(inviter)
	if err != nil {
		return err
	}
	for _, done := range user.Rewarded {
		if done == invitee {
			return nil
		}
	}
	user.Rewarded = append(user.Rewarded, invitee)
	user.Credit += referralRewardPages
	return s.saveLocked()
}

// reserve takes up to pages from the balance for a new order and returns how many.
func (s *ReferralStore) reserve(openid string, pages int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[openid]
	if !ok || user.Credit <= 0 || pages <= 0 {
		return 0, nil
	}
	used := min(user.Credit, pages)
	user.Credit -= used
	if err := s.saveLocked(); err != nil {
		user.Credit += used
		return 0, err
	}
	return used, nil
}

// restore returns pages reserved by an order that was closed or refunded.
func (s *ReferralStore) restore(openid string, pages int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, err := s.userLocked(openid)
	if err != nil {
		return err
	}
	user.Credit += pages
	if err := s.saveLocked(); err != nil {
		user.Credit -= pages
		return err
	}
	return nil
}

// returnOrder persists the balance and its idempotency key in the same file.
// If saving the order flag fails afterwards, replaying this entry is harmless.
func (s *ReferralStore) returnOrder(openid, orderID string, pages int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, err := s.userLocked(openid)
	if err != nil {
		return err
	}
	if user.Returned[orderID] {
		return nil
	}
	if user.Returned == nil {
		user.Returned = make(map[string]bool)
	}
	user.Returned[orderID] = true
	user.Credit += pages
	if err := s.saveLocked(); err != nil {
		delete(user.Returned, orderID)
		user.Credit -= pages
		return err
	}
	return nil
}

func (s *Server) handleReferral(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.ordersEnabled() {
		writeError(w, 503, "邀请功能暂未开放")
		return
	}
	openid, _ := r.Context().Value(ctxKeyOpenID).(string)
	user, err := s.pay.referrals.view(openid)
	if err != nil {
		writeError(w, 500, "邀请信息暂不可用")
		return
	}
	writeJSON(w, 200, map[string]any{"code": user.Code, "credit_pages": user.Credit, "invited": len(user.Rewarded),
		"reward_pages": referralRewardPages})
}

// settleReferrals rewards inviters for delivered trials and returns credit held by
// orders that ended unpaid or refunded. Both are idempotent and run on every sweep.
func (s *Server) settleReferrals(order PayOrder) {
	if order.Trial && order.ReferredBy != "" && order.Delivery == "archived" {
		if err := s.pay.referrals.reward(order.ReferredBy, order.OpenID); err != nil {
			logReferral("reward", order.ID, err)
		}
	}
	if order.CreditPages > 0 && !order.CreditReturned && order.creditReleased() {
		// Hold the order lock through the ledger write so payment cannot race a return.
		err := s.pay.store.update(order.ID, func(current *PayOrder) error {
			if current.CreditReturned || !current.creditReleased() {
				return nil
			}
			if err := s.pay.referrals.returnOrder(current.OpenID, current.ID, current.CreditPages); err != nil {
				return err
			}
			current.CreditReturned = true
			return nil
		})
		if err != nil {
			logReferral("credit return", order.ID, err)
		}
	}
}

func logReferral(action, orderID string, err error) {
	log.Printf("referral %s failed (order=%s): %v", action, orderID, err)
}
