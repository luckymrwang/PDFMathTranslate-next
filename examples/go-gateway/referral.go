package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"log"
	"net/http"
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
type ReferralStore struct{ db *sql.DB }

func openReferralStore(dir string) (*ReferralStore, error) {
	db, err := openDatabase(dir)
	if err != nil {
		return nil, err
	}
	return &ReferralStore{db: db}, nil
}

func referralUserTx(tx *databaseTx, owner string) (referralUser, error) {
	var user referralUser
	err := tx.QueryRow("SELECT code,credit FROM referral_users WHERE openid=?", owner).Scan(&user.Code, &user.Credit)
	if !errors.Is(err, sql.ErrNoRows) {
		return user, err
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return user, err
		}
		for i, b := range raw {
			raw[i] = alphabet[int(b)%len(alphabet)]
		}
		user.Code = string(raw[:])
		result, err := tx.Exec("INSERT INTO referral_users(openid,code,credit) VALUES(?,?,0) ON CONFLICT(code) DO NOTHING", owner, user.Code)
		if err != nil {
			return user, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return user, err
		}
		if n == 1 {
			return user, nil
		}
	}
}

func (s *ReferralStore) view(owner string) (referralUser, error) {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return referralUser{}, err
	}
	defer tx.Rollback()
	user, err := referralUserTx(tx, owner)
	if err != nil {
		return user, err
	}
	rows, err := tx.Query("SELECT invitee FROM referral_rewards WHERE inviter=? ORDER BY invitee", owner)
	if err != nil {
		return user, err
	}
	for rows.Next() {
		var invitee string
		if err := rows.Scan(&invitee); err != nil {
			rows.Close()
			return user, err
		}
		user.Rewarded = append(user.Rewarded, invitee)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return user, err
	}
	return user, tx.Commit()
}

func (s *ReferralStore) credit(owner string) int {
	var credit int
	_ = s.db.QueryRow("SELECT credit FROM referral_users WHERE openid=?", owner).Scan(&credit)
	return credit
}

func (s *ReferralStore) inviter(code, invitee string) string {
	var owner string
	_ = s.db.QueryRow("SELECT openid FROM referral_users WHERE code=? AND openid<>?", code, invitee).Scan(&owner)
	return owner
}

func rewardTx(tx *databaseTx, inviter, invitee string) error {
	if _, err := referralUserTx(tx, inviter); err != nil {
		return err
	}
	result, err := tx.Exec("INSERT INTO referral_rewards(inviter,invitee) VALUES(?,?) ON CONFLICT(invitee) DO NOTHING", inviter, invitee)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err = tx.Exec("UPDATE referral_users SET credit=credit+? WHERE openid=?", referralRewardPages, inviter)
	return err
}

func (s *ReferralStore) reserve(owner string, pages int) (int, error) {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var balance int
	err = tx.QueryRow("SELECT credit FROM referral_users WHERE openid=?", owner).Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	used := min(balance, max(0, pages))
	if _, err := tx.Exec("UPDATE referral_users SET credit=credit-? WHERE openid=?", used, owner); err != nil {
		return 0, err
	}
	return used, tx.Commit()
}

func (s *ReferralStore) restore(owner string, pages int) error {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := referralUserTx(tx, owner); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE referral_users SET credit=credit+? WHERE openid=?", pages, owner); err != nil {
		return err
	}
	return tx.Commit()
}

func returnOrderTx(tx *databaseTx, owner, orderID string, pages int) error {
	if _, err := referralUserTx(tx, owner); err != nil {
		return err
	}
	result, err := tx.Exec("INSERT INTO referral_returns(order_id,openid) VALUES(?,?) ON CONFLICT(order_id) DO NOTHING", orderID, owner)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err = tx.Exec("UPDATE referral_users SET credit=credit+? WHERE openid=?", pages, owner)
	return err
}

// Settlement and the state that causes it are committed together.
func settleOrderTx(tx *databaseTx, order *PayOrder) error {
	if order.Trial && order.ReferredBy != "" && order.Delivery == "archived" {
		if err := rewardTx(tx, order.ReferredBy, order.OpenID); err != nil {
			return err
		}
	}
	if order.CreditPages > 0 && !order.CreditReturned && order.creditReleased() {
		if err := returnOrderTx(tx, order.OpenID, order.ID, order.CreditPages); err != nil {
			return err
		}
		order.CreditReturned = true
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
	// Already rewarded deliveries need no further write. Failed reads are retried
	// by the normal transaction instead of being mistaken for a settled trial.
	if order.Trial && order.ReferredBy != "" && order.Delivery == "archived" {
		var inviter string
		err := s.pay.store.db.QueryRow("SELECT inviter FROM referral_rewards WHERE invitee=?", order.OpenID).Scan(&inviter)
		if err == nil {
			return
		}
	}

	if (order.Trial && order.ReferredBy != "" && order.Delivery == "archived") ||
		(order.CreditPages > 0 && !order.CreditReturned && order.creditReleased()) {
		if err := s.pay.store.update(order.ID, func(*PayOrder) error { return nil }); err != nil {
			logReferral("settlement", order.ID, err)
		}
	}
}

func logReferral(action, orderID string, err error) {
	log.Printf("referral %s failed (order=%s): %v", action, orderID, err)
}
