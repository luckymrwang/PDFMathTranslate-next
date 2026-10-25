package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// The gateway is the sole writer of this directory. Orders and referral credits
// share one database; uploaded PDFs and archived results remain ordinary files.
func openDatabase(dir string) (*sql.DB, error) {
	for _, name := range []string{"", "uploads", "results"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(dir, "gateway.db")
	// Precreate privately, including when the process has a permissive umask.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout%3D5000&_pragma=foreign_keys%3DON&_pragma=synchronous%3DFULL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL"} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > 1 {
		return nil, fmt.Errorf("unsupported database version %d", version)
	}
	tx, err := beginDatabaseTx(db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS orders (
  id TEXT PRIMARY KEY,
  record TEXT NOT NULL CHECK(json_valid(record)),
  openid TEXT GENERATED ALWAYS AS (json_extract(record, '$.OpenID')) VIRTUAL,
  state TEXT GENERATED ALWAYS AS (json_extract(record, '$.State')) VIRTUAL,
  wx_order_id TEXT GENERATED ALWAYS AS (json_extract(record, '$.WxOrderID')) VIRTUAL,
  created_at INTEGER GENERATED ALWAYS AS (json_extract(record, '$.CreatedAt')) VIRTUAL,
  CHECK(id = json_extract(record, '$.ID'))
 );
 CREATE INDEX IF NOT EXISTS orders_owner_created ON orders(openid, created_at);
 CREATE INDEX IF NOT EXISTS orders_state ON orders(state);
 CREATE UNIQUE INDEX IF NOT EXISTS orders_platform_id ON orders(wx_order_id) WHERE wx_order_id <> '';
 CREATE TABLE IF NOT EXISTS referral_users (
  openid TEXT PRIMARY KEY,
  code TEXT NOT NULL UNIQUE CHECK(length(code)=8),
  credit INTEGER NOT NULL DEFAULT 0 CHECK(credit >= 0)
 );
 CREATE TABLE IF NOT EXISTS referral_rewards (
  invitee TEXT PRIMARY KEY,
  inviter TEXT NOT NULL REFERENCES referral_users(openid),
  CHECK(invitee <> inviter)
 );
 CREATE TABLE IF NOT EXISTS referral_returns (
  order_id TEXT PRIMARY KEY,
  openid TEXT NOT NULL REFERENCES referral_users(openid)
 );
 PRAGMA user_version=1;`)
	if err != nil {
		return nil, err
	}
	var migrated string
	err = tx.QueryRow("SELECT value FROM metadata WHERE key='json_imported'").Scan(&migrated)
	if errors.Is(err, sql.ErrNoRows) {
		if err := importJSON(tx, dir); err != nil {
			return nil, fmt.Errorf("legacy JSON migration: %w", err)
		}
		if _, err := tx.Exec("INSERT INTO metadata(key,value) VALUES('json_imported','1')"); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ok = true
	return db, nil
}

// Import both ledgers in a single transaction. Never delete or rewrite the
// legacy files, and never import them again after the marker is committed.
func importJSON(tx *databaseTx, dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "orders", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var order PayOrder
		if err := json.Unmarshal(data, &order); err != nil {
			return fmt.Errorf("invalid order file: %s", filepath.Base(path))
		}
		if order.ID == "" || order.ID+".json" != filepath.Base(path) {
			return errors.New("invalid order identity")
		}
		if err := writeOrder(tx, order); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "referrals.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var users map[string]*referralUser
	if err := json.Unmarshal(data, &users); err != nil {
		return errors.New("invalid referral file")
	}
	for owner, user := range users {
		if user == nil {
			return errors.New("invalid referral user")
		}
		if _, err := tx.Exec("INSERT INTO referral_users(openid,code,credit) VALUES(?,?,?)", owner, user.Code, user.Credit); err != nil {
			return err
		}
	}
	for owner, user := range users {
		for _, invitee := range user.Rewarded {
			if _, err := tx.Exec("INSERT INTO referral_rewards(inviter,invitee) VALUES(?,?)", owner, invitee); err != nil {
				return err
			}
		}
		for orderID, returned := range user.Returned {
			if returned {
				if _, err := tx.Exec("INSERT INTO referral_returns(order_id,openid) VALUES(?,?)", orderID, owner); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func writeOrder(tx *databaseTx, order PayOrder) error {
	data, err := json.Marshal(order)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO orders(id,record) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record", order.ID, string(data))
	return err
}

// Keep the connection pinned until COMMIT or ROLLBACK actually completes.
// SQLite leaves a transaction active when COMMIT fails (e.g. deferred FK).
// database/sql.Tx marks itself done even then, so use explicit SQL here.
type databaseTx struct {
	conn *sql.Conn
	done bool
}

func beginDatabaseTx(db *sql.DB) (*databaseTx, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, err
	}
	return &databaseTx{conn: conn}, nil
}
func (tx *databaseTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(context.Background(), query, args...)
}
func (tx *databaseTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(context.Background(), query, args...)
}
func (tx *databaseTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(context.Background(), query, args...)
}
func (tx *databaseTx) Commit() error {
	if tx.done {
		return sql.ErrTxDone
	}
	_, err := tx.Exec("COMMIT")
	if err != nil {
		tx.Rollback()
		return err
	}
	tx.done = true
	return tx.conn.Close()
}
func (tx *databaseTx) Rollback() error {
	if tx.done {
		return nil
	}
	_, err := tx.Exec("ROLLBACK")
	tx.done = true
	if err != nil {
		_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	closeErr := tx.conn.Close()
	if err != nil {
		return err
	}
	return closeErr
}
