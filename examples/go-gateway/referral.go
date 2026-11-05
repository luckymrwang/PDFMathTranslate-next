package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"
)

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
	if _, err = tx.Exec("INSERT INTO points_ledger(event_key,openid,delta,day,kind) VALUES(?,?,?,'','return')", "return:"+orderID, owner, pages); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE referral_users SET credit=credit+? WHERE openid=?", pages, owner)
	return err
}

// Settlement and the state that causes it are committed together.
func settleOrderTx(tx *databaseTx, order *PayOrder) error {
	if order.CreditPoints > 0 && !order.CreditReturned && order.creditReleased() {
		if err := returnOrderTx(tx, order.OpenID, order.ID, order.CreditPoints); err != nil {
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
	wallet, err := s.pay.referrals.wallet(openid, time.Now())
	if err != nil {
		writeError(w, 500, "积分信息暂不可用")
		return
	}
	wallet["code"], wallet["invited"] = user.Code, len(user.Rewarded)
	writeJSON(w, 200, wallet)
}

func (s *Server) settleReferrals(order PayOrder) {
	if order.CreditPoints > 0 && !order.CreditReturned && order.creditReleased() {
		if err := s.pay.store.update(order.ID, func(*PayOrder) error { return nil }); err != nil {
			logReferral("settlement", order.ID, err)
		}
	}
}

func logReferral(action, orderID string, err error) {
	log.Printf("referral %s failed (order=%s): %v", action, orderID, err)
}

const pointsPerPage = 10
const dailyPointsLimit = 500
const pointsReward = 100

var chinaTime = time.FixedZone("Asia/Shanghai", 8*60*60)

func pointsDay(now time.Time) string { return now.In(chinaTime).Format("2006-01-02") }

func migratePoints(tx *databaseTx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS points_logins(openid TEXT PRIMARY KEY, first_day TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS points_daily(openid TEXT NOT NULL,day TEXT NOT NULL,earned INTEGER NOT NULL DEFAULT 0,claimed INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(openid,day));
 CREATE TABLE IF NOT EXISTS points_ledger(event_key TEXT PRIMARY KEY,openid TEXT NOT NULL,delta INTEGER NOT NULL,day TEXT NOT NULL,kind TEXT NOT NULL);
 UPDATE referral_users SET credit=credit*10;
 INSERT INTO points_ledger(event_key,openid,delta,day,kind) SELECT 'migration:'||openid,openid,credit,'','migration' FROM referral_users;
 INSERT OR IGNORE INTO points_logins SELECT openid,'' FROM referral_users;
 INSERT OR IGNORE INTO points_logins SELECT openid,'' FROM orders WHERE openid<>'';
 INSERT OR IGNORE INTO points_logins SELECT invitee,'' FROM referral_rewards;
 INSERT OR IGNORE INTO points_logins SELECT json_extract(record,'$.ReferredBy'),'' FROM orders WHERE json_extract(record,'$.ReferredBy')<>'';
 UPDATE orders SET record=json_set(record,'$.CreditPoints',coalesce(json_extract(record,'$.CreditPages'),0)*10);
 PRAGMA user_version=2;`)
	if err != nil {
		return err
	}
	return nil
}

// Every earning event is recorded even if capped, so it cannot be replayed tomorrow.
func earnPointsTx(tx *databaseTx, owner, event, day, kind string, claim bool) (int, error) {
	if _, err := referralUserTx(tx, owner); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO points_daily(openid,day) VALUES(?,?)", owner, day); err != nil {
		return 0, err
	}
	var earned int
	if err := tx.QueryRow("SELECT earned FROM points_daily WHERE openid=? AND day=?", owner, day).Scan(&earned); err != nil {
		return 0, err
	}
	awarded := min(pointsReward, max(0, dailyPointsLimit-earned))
	result, err := tx.Exec("INSERT OR IGNORE INTO points_ledger(event_key,openid,delta,day,kind) VALUES(?,?,?,?,?)", event, owner, awarded, day, kind)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	claimed := 0
	if claim {
		claimed = 1
	}
	if _, err = tx.Exec("UPDATE points_daily SET earned=earned+?,claimed=max(claimed,?) WHERE openid=? AND day=?", awarded, claimed, owner, day); err != nil {
		return 0, err
	}
	_, err = tx.Exec("UPDATE referral_users SET credit=credit+? WHERE openid=?", awarded, owner)
	return awarded, err
}
func (s *ReferralStore) login(owner, ref string, now time.Time) error {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("INSERT OR IGNORE INTO points_logins(openid,first_day) VALUES(?,?)", owner, pointsDay(now))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 && ref != "" {
		var inviter string
		err = tx.QueryRow("SELECT openid FROM referral_users WHERE code=? AND openid<>?", ref, owner).Scan(&inviter)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err = tx.Exec("INSERT INTO referral_rewards(inviter,invitee) VALUES(?,?)", inviter, owner); err != nil {
				return err
			}
			for _, who := range []string{owner, inviter} {
				if _, err = earnPointsTx(tx, who, "invite:"+owner+":"+who, pointsDay(now), "invite", false); err != nil {
					return err
				}
			}
		}
	}
	if _, err = referralUserTx(tx, owner); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *ReferralStore) claim(owner string, now time.Time) (int, error) {
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	day := pointsDay(now)
	awarded, err := earnPointsTx(tx, owner, "daily:"+owner+":"+day, day, "daily", true)
	if err != nil {
		return 0, err
	}
	return awarded, tx.Commit()
}
func (s *ReferralStore) wallet(owner string, now time.Time) (map[string]any, error) {
	var balance, earned, claimed int
	tx, err := beginDatabaseTx(s.db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	user, err := referralUserTx(tx, owner)
	if err != nil {
		return nil, err
	}
	balance = user.Credit
	err = tx.QueryRow("SELECT earned,claimed FROM points_daily WHERE openid=? AND day=?", owner, pointsDay(now)).Scan(&earned, &claimed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return map[string]any{"points_balance": balance, "daily_earned": earned, "daily_limit": dailyPointsLimit, "daily_claimed": claimed != 0, "daily_reward": pointsReward, "reward_points": pointsReward, "points_per_page": pointsPerPage, "points_enabled": true}, tx.Commit()
}
func (s *Server) handlePointsClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	if !s.ordersEnabled() {
		writeError(w, 503, "积分暂未开放")
		return
	}
	owner, _ := r.Context().Value(ctxKeyOpenID).(string)
	now := time.Now()
	awarded, err := s.pay.referrals.claim(owner, now)
	if err != nil {
		writeError(w, 500, "积分领取失败")
		return
	}
	wallet, err := s.pay.referrals.wallet(owner, now)
	if err != nil {
		writeError(w, 500, "积分信息暂不可用")
		return
	}
	wallet["awarded"] = awarded
	writeJSON(w, 200, wallet)
}
