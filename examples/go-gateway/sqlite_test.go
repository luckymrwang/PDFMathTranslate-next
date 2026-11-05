package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func legacyJSON(t *testing.T, dir, name string, value any) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteMigrationPreservesRecordsAndImportsOnce(t *testing.T) {
	dir := t.TempDir()
	order := PayOrder{ID: "legacy", OpenID: "owner", WxOrderID: "wx", State: "submitted", Pages: 12, Quantity: 9, Total: 90,
		TrialCredit: 3, UpgradeFrom: "trial", CreditPages: 2, TaskID: "task", TaskHistory: []string{"old-task"},
		FilePath: "/data/uploads/file.pdf", MonoKey: "archive/mono", DualFile: "/data/results/dual.pdf", Delivery: "archived",
		Provided: true, CreatedAt: 123, LastQuery: 456, SignData: "signed", Attempts: 2, DocumentPages: 12}
	legacyJSON(t, dir, "orders/legacy.json", order)
	legacyJSON(t, dir, "referrals.json", map[string]referralUser{"owner": {Code: "ABCDEFGH", Credit: 7, Rewarded: []string{"friend"}, Returned: map[string]bool{"returned": true}}})
	store, err := openOrderStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	order.CreditPoints = 20
	got, _ := store.get(order.ID)
	if !reflect.DeepEqual(got, order) {
		t.Fatal(got)
	}
	refs := &ReferralStore{db: store.db}
	user, err := refs.view("owner")
	if err != nil || user.Credit != 70 || len(user.Rewarded) != 1 || refs.inviter("ABCDEFGH", "another") != "owner" {
		t.Fatal(user, err)
	}
	if err := store.update(order.ID, func(o *PayOrder) error { o.Provided = false; return nil }); err != nil {
		t.Fatal(err)
	}
	store.db.Close()
	// A committed migration must ignore even malformed or stale legacy files.
	os.WriteFile(filepath.Join(dir, "orders/legacy.json"), []byte("invalid"), 0600)
	reopened, err := openOrderStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	got, _ = reopened.get(order.ID)
	if got.Provided || len(reopened.all()) != 1 {
		t.Fatal(got)
	}
	var integrity string
	if err := reopened.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
}

func TestSQLiteMigrationFailureRollsBackAndCanRetry(t *testing.T) {
	for _, bad := range []string{"invalid-json", "wrong-id", "duplicate-wx", "invalid-referral"} {
		t.Run(bad, func(t *testing.T) {
			dir := t.TempDir()
			legacyJSON(t, dir, "orders/a.json", PayOrder{ID: "a", WxOrderID: "same"})
			switch bad {
			case "invalid-json":
				os.WriteFile(filepath.Join(dir, "orders/b.json"), []byte("{"), 0600)
			case "wrong-id":
				legacyJSON(t, dir, "orders/b.json", PayOrder{ID: "c"})
			case "duplicate-wx":
				legacyJSON(t, dir, "orders/b.json", PayOrder{ID: "b", WxOrderID: "same"})
			case "invalid-referral":
				legacyJSON(t, dir, "referrals.json", map[string]any{"owner": nil})
			}
			if db, err := openDatabase(dir); err == nil {
				db.Close()
				t.Fatal("accepted invalid migration")
			}
			os.Remove(filepath.Join(dir, "orders/b.json"))
			os.Remove(filepath.Join(dir, "referrals.json"))
			store, err := openOrderStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.db.Close()
			if len(store.all()) != 1 {
				t.Fatal(store.all())
			}
		})
	}
}

func TestSQLiteMigrationRepairsLegacyPartialReturn(t *testing.T) {
	dir := t.TempDir()
	legacyJSON(t, dir, "orders/returned.json", PayOrder{ID: "returned", OpenID: "owner", State: "refunded", CreditPages: 3})
	legacyJSON(t, dir, "referrals.json", map[string]referralUser{"owner": {Code: "ABCDEFGH", Credit: 5, Returned: map[string]bool{"returned": true}}})
	store, err := openOrderStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	if err := store.update("returned", func(*PayOrder) error { return nil }); err != nil {
		t.Fatal(err)
	}
	order, _ := store.get("returned")
	if !order.CreditReturned || (&ReferralStore{db: store.db}).credit("owner") != 50 {
		t.Fatal(order)
	}
}

func TestSQLitePurchaseRollbackAndConcurrentReservations(t *testing.T) {
	s := testPayServer(t)
	if err := s.pay.referrals.restore("owner", 50); err != nil {
		t.Fatal(err)
	}
	price := func(o *PayOrder, n int) error { o.Quantity = n; o.Total = n * 10; return nil }
	// Fail after the balance debit, at order insertion.
	if _, err := s.pay.store.db.Exec(`CREATE TRIGGER fail_insert BEFORE INSERT ON orders BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	order := PayOrder{ID: "failed", OpenID: "owner", Engine: "Bing"}
	if err := s.pay.store.addPurchase(&order, 3, price); err == nil {
		t.Fatal("accepted failed insert")
	}
	if s.pay.referrals.credit("owner") != 50 || len(s.pay.store.all()) != 0 || order.CreditPages != 0 {
		t.Fatal("debit survived failure")
	}
	s.pay.store.db.Exec("DROP TRIGGER fail_insert")
	if err := s.pay.store.addPurchase(&order, 3, func(*PayOrder, int) error { return errors.New("price failed") }); err == nil || s.pay.referrals.credit("owner") != 50 {
		t.Fatal("price failure not rolled back")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := PayOrder{ID: fmt.Sprint(i), OpenID: "owner", Engine: "Bing"}
			if err := s.pay.store.addPurchase(&o, 3, price); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, o := range s.pay.store.all() {
		total += o.CreditPages
	}
	if total != 5 || s.pay.referrals.credit("owner") != 0 {
		t.Fatal(total)
	}
}

// A deferred foreign-key violation fails COMMIT after all statements succeed.
func TestSQLiteCommitFailureLeavesOrderAndBalanceUnchanged(t *testing.T) {
	s := testPayServer(t)
	if err := s.pay.referrals.restore("owner", 50); err != nil {
		t.Fatal(err)
	}
	_, err := s.pay.store.db.Exec(`CREATE TABLE commit_parent(id INTEGER PRIMARY KEY);
 CREATE TABLE commit_child(id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER fail_commit AFTER INSERT ON orders BEGIN INSERT INTO commit_child VALUES(1); END;`)
	if err != nil {
		t.Fatal(err)
	}
	order := PayOrder{ID: "commit-failed", OpenID: "owner", Engine: "Bing"}
	err = s.pay.store.addPurchase(&order, 3, func(o *PayOrder, n int) error { o.Total = n * 10; return nil })
	if err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	if s.pay.referrals.credit("owner") != 50 || len(s.pay.store.all()) != 0 || order.CreditPages != 0 {
		t.Fatal("failed commit published data")
	}
	var count int
	if err := s.pay.store.db.QueryRow("SELECT count(*) FROM orders").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestHistoricalArchivedTrialNoLongerRewards(t *testing.T) {
	s := testPayServer(t)
	order := PayOrder{ID: "old", OpenID: "friend", Trial: true, ReferredBy: "inviter", State: "submitted"}
	if err := s.pay.store.add(order); err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.update(order.ID, func(o *PayOrder) error { o.Delivery = "archived"; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.pay.referrals.credit("inviter") != 0 {
		t.Fatal("historical trial rewarded")
	}
}

func TestSQLiteConcurrentTrialAndPlatformIdentity(t *testing.T) {
	s := testPayServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.pay.store.addTrial(PayOrder{ID: fmt.Sprint(i), OpenID: "owner", Trial: true})
			if err != nil && !errors.Is(err, errTrialUsed) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(s.pay.store.all()) != 1 {
		t.Fatal(s.pay.store.all())
	}
	// The database enforces platform identity even when bypassing the cache checks.
	for _, id := range []string{"a", "b"} {
		if err := s.pay.store.add(PayOrder{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.pay.store.update("a", func(o *PayOrder) error { o.WxOrderID = "unique"; return nil }); err != nil {
		t.Fatal(err)
	}
	s.pay.store.mu.Lock()
	err := s.pay.store.saveLocked(PayOrder{ID: "b", WxOrderID: "unique"})
	s.pay.store.mu.Unlock()
	if err == nil {
		t.Fatal("duplicate platform id accepted")
	}
}

func TestSQLiteInvitationLookupFailureDoesNotConsumeTrial(t *testing.T) {
	s := testPayServer(t)
	if _, err := s.pay.store.db.Exec("ALTER TABLE referral_users RENAME TO hidden_users"); err != nil {
		t.Fatal(err)
	}
	order := PayOrder{ID: "trial", OpenID: "friend", Trial: true}
	if err := s.pay.store.addInvitedTrial(order, "ABCDEFGH"); err == nil {
		t.Fatal("ignored referral lookup failure")
	}
	if s.pay.store.hasTrial("friend") {
		t.Fatal("failed lookup consumed trial")
	}
	if _, err := s.pay.store.db.Exec("ALTER TABLE hidden_users RENAME TO referral_users"); err != nil {
		t.Fatal(err)
	}
	inviter, err := s.pay.referrals.view("inviter")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pay.store.addInvitedTrial(order, inviter.Code); err != nil {
		t.Fatal(err)
	}
	current, _ := s.pay.store.get(order.ID)
	if current.ReferredBy != "inviter" {
		t.Fatal(current)
	}
	if err := s.pay.store.update(order.ID, func(o *PayOrder) error { o.Delivery = "archived"; return nil }); err != nil {
		t.Fatal(err)
	}
	// An already rewarded delivery must not attempt another order write.
	if _, err := s.pay.store.db.Exec(`CREATE TRIGGER no_repeat BEFORE UPDATE ON orders BEGIN SELECT RAISE(ABORT,'repeat write'); END`); err != nil {
		t.Fatal(err)
	}
	current, _ = s.pay.store.get(order.ID)
	s.settleReferrals(current)
	if s.pay.referrals.credit("inviter") != 0 {
		t.Fatal("incorrect balance")
	}
}

func TestSQLiteReplacementConnectionKeepsPragmas(t *testing.T) {
	s := testPayServer(t)
	conn, err := s.pay.store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	conn.Close()
	for name, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000, "synchronous": 2} {
		var got int
		if err := s.pay.store.db.QueryRow("PRAGMA " + name).Scan(&got); err != nil || got != want {
			t.Fatal(name, got, err)
		}
	}
}

func TestVersionOnePointsMigrationSeedsOldUsers(t *testing.T) {
	dir := t.TempDir()
	db, err := openDatabase(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE metadata SET value='1' WHERE key='json_imported';
 INSERT INTO referral_users(openid,code,credit) VALUES('host','ABCDEFGH',7);
 INSERT INTO referral_rewards(inviter,invitee) VALUES('host','friend');
 INSERT INTO orders(id,record) VALUES('old','{"ID":"old","OpenID":"buyer","CreditPages":3,"State":"pending"}');
 DELETE FROM points_logins; PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := openOrderStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := &ReferralStore{db: store.db}
	for _, owner := range []string{"host", "friend", "buyer"} {
		if err := refs.login(owner, "ABCDEFGH", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if refs.credit("host") != 70 || refs.credit("friend") != 0 {
		t.Fatal("old users rewarded or conversion wrong")
	}
	o, _ := store.get("old")
	if o.CreditPoints != 30 {
		t.Fatal(o)
	}
	store.db.Close()
	store, err = openOrderStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	refs = &ReferralStore{db: store.db}
	if refs.credit("host") != 70 {
		t.Fatal("conversion repeated")
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatal("version marker regressed", version, err)
	}
	if err := store.update("old", func(o *PayOrder) error { o.State = "closed"; o.PaymentClosed = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if refs.credit("buyer") != 30 {
		t.Fatal("reserved migration return")
	}
}
