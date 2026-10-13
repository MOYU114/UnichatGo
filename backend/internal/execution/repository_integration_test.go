package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"unichatgo/internal/storage"
)

func mysqlRepository(t *testing.T) (*Repository, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UNICHATGO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("real MySQL not executed: UNICHATGO_TEST_MYSQL_DSN is unset")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(24)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err = storage.Migrate(db, "mysql"); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewRepository(db, Limits{AcceptRPM: 1000, UserQueue: 1000, SessionQueue: 1000, TaskTimeout: time.Hour})
	user, err := db.Exec(`INSERT INTO users(username,password_hash,created_at) VALUES(?,'test-only',NOW())`, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := user.LastInsertId()
	sid := testSession(t, r, uid)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE p FROM execution_attachment_pins p JOIN execution_evidence e ON e.id=p.request_id WHERE e.user_id=?`, uid)
		_, _ = db.Exec(`DELETE q FROM execution_quota_calls q JOIN execution_evidence e ON e.id=q.request_id WHERE e.user_id=?`, uid)
		_, _ = db.Exec(`DELETE FROM users WHERE id=?`, uid)
	})
	return r, uid, sid
}
func testSession(t *testing.T, r *Repository, uid int64) int64 {
	t.Helper()
	res, err := r.DB.Exec(`INSERT INTO sessions(user_id,title,created_at,updated_at) VALUES(?,'isolated test',NOW(),NOW())`, uid)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}
func testInput(uid, sid int64, key, content string) Input {
	return Input{UserID: uid, SessionID: sid, ClientID: key, Content: content, PayloadHash: HashPayload([]byte(content)), Provider: "mock", Model: "test", Snapshot: "encrypted-test-only", QuotaScope: fmt.Sprintf("isolated:%d", uid)}
}
func testPolicy() QuotaPolicy {
	return QuotaPolicy{Platform: QuotaLimit{Concurrency: 1000, RPM: 100000, TPM: 100000000}, User: QuotaLimit{Concurrency: 100, RPM: 10000, TPM: 10000000}, Credential: QuotaLimit{Concurrency: 100, RPM: 10000, TPM: 10000000}, Grace: time.Minute}
}
func acceptTest(t *testing.T, r *Repository, in Input) Receipt {
	t.Helper()
	e, _, err := r.Accept(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func claimTest(t *testing.T, r *Repository, id string) Evidence {
	t.Helper()
	e, err := r.Claim(context.Background(), id, uuid.NewString(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func completeTest(t *testing.T, r *Repository, e Evidence, text string) {
	t.Helper()
	ctx := context.Background()
	if err := r.StartCall(ctx, e, testPolicy(), 100); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, e, Completed, text, ""); err != nil {
		t.Fatal(err)
	}
}
func expireLease(t *testing.T, r *Repository, id string) {
	t.Helper()
	if _, err := r.DB.Exec(`UPDATE execution_evidence SET lease_until_ms=`+sqlNow+`-1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAcceptanceIdentityAndRestart(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	in := testInput(uid, sid, "CaseSensitive-ID", "hello")
	const count = 16
	start := make(chan struct{})
	results := make(chan Receipt, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			other := NewRepository(r.DB, r.limits)
			receipt, _, err := other.Accept(ctx, in)
			results <- receipt
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for receipt := range results {
		if id == "" {
			id = receipt.ID
		}
		if receipt.ID != id {
			t.Fatalf("split identity %q != %q", id, receipt.ID)
		}
	}
	var messages, outbox int
	if err := r.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, sid).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := r.DB.QueryRow(`SELECT COUNT(*) FROM execution_outbox WHERE request_id=?`, id).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || outbox != 1 {
		t.Fatalf("messages=%d outbox=%d", messages, outbox)
	}
	changed := in
	changed.PayloadHash = HashPayload([]byte("different"))
	if _, _, err := r.Accept(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	changed = in
	changed.UserID++
	if _, err := r.Lookup(ctx, changed.UserID, sid, in.ClientID, in.PayloadHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership bypass: %v", err)
	}
	completeTest(t, r, claimTest(t, r, id), "answer")
	other := NewRepository(r.DB, r.limits)
	receipt, replay, err := other.Accept(ctx, in)
	if err != nil || !replay || receipt.ID != id || receipt.State != Completed {
		t.Fatalf("completed restart replay: %+v %v %v", receipt, replay, err)
	}
	lower := in
	lower.ClientID = strings.ToLower(in.ClientID)
	second := acceptTest(t, r, lower)
	if second.ID == id {
		t.Fatal("binary client key collapsed case")
	}
}

func TestMySQLContextSequenceAndLegacyPrefix(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	if _, err := r.DB.Exec(`INSERT INTO messages(user_id,session_id,role,content,created_at) VALUES(?,?,'user','legacy',NOW()),(?,?,'assistant','legacy-answer',NOW())`, uid, sid, uid, sid); err != nil {
		t.Fatal(err)
	}
	one := acceptTest(t, r, testInput(uid, sid, "one", "first"))
	two := acceptTest(t, r, testInput(uid, sid, "two", "second"))
	three := acceptTest(t, r, testInput(uid, sid, "three", "future"))
	e1 := claimTest(t, r, one.ID)
	if _, err := r.Claim(ctx, two.ID, "other", time.Minute); !errors.Is(err, ErrNotHead) {
		t.Fatalf("second entered model: %v", err)
	}
	otherSession := testSession(t, r, uid)
	parallel := acceptTest(t, r, testInput(uid, otherSession, "parallel", "other-session"))
	_ = claimTest(t, r, parallel.ID)
	messages, err := r.Context(ctx, e1)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[2].Content != "first" {
		t.Fatalf("future contamination %+v", messages)
	}
	completeTest(t, r, e1, "first-answer")
	e2 := claimTest(t, r, two.ID)
	messages, err = r.Context(ctx, e2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"legacy", "legacy-answer", "first", "first-answer", "second"}
	if len(messages) != len(want) {
		t.Fatalf("context=%+v", messages)
	}
	for i, m := range messages {
		if m.Content != want[i] {
			t.Fatalf("position %d got %s want %s", i, m.Content, want[i])
		}
	}
	if err = r.Finish(ctx, e2, Failed, "", "internal_failed"); err != nil {
		t.Fatal(err)
	}
	e3 := claimTest(t, r, three.ID)
	messages, err = r.Context(ctx, e3)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Content == "second" {
			t.Fatal("failed turn polluted context")
		}
	}
}

func TestMySQLCrashFenceAndUncertain(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	one := acceptTest(t, r, testInput(uid, sid, "one", "first"))
	two := acceptTest(t, r, testInput(uid, sid, "two", "second"))
	old := claimTest(t, r, one.ID)
	expireLease(t, r, one.ID)
	if _, err := r.Recover(ctx, 100); err != nil {
		t.Fatal(err)
	}
	current := claimTest(t, r, one.ID)
	if current.Fence <= old.Fence {
		t.Fatal("fence did not advance")
	}
	if err := r.StartCall(ctx, old, testPolicy(), 100); !errors.Is(err, ErrStale) {
		t.Fatalf("old started: %v", err)
	}
	if err := r.StartCall(ctx, current, testPolicy(), 100); err != nil {
		t.Fatal(err)
	}
	expireLease(t, r, one.ID)
	if _, err := r.Recover(ctx, 100); err != nil {
		t.Fatal(err)
	}
	e, err := r.Get(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != Uncertain {
		t.Fatalf("post-call state=%s", e.State)
	}
	if err = r.Finish(ctx, current, Completed, "stale reply", ""); !errors.Is(err, ErrStale) {
		t.Fatalf("stale write: %v", err)
	}
	if _, err = r.Claim(ctx, two.ID, "worker", time.Minute); !errors.Is(err, ErrNotHead) {
		t.Fatalf("uncertain did not block: %v", err)
	}
	if _, _, err = r.Accept(ctx, testInput(uid, sid, "three", "third")); !errors.Is(err, ErrBlocked) {
		t.Fatalf("uncertain accepted: %v", err)
	}
	if err = r.DeleteSession(ctx, uid, sid); !errors.Is(err, ErrBusy) {
		t.Fatalf("deleted uncertain session: %v", err)
	}
	if err = r.DeleteUser(ctx, uid); !errors.Is(err, ErrBusy) {
		t.Fatalf("deleted uncertain user: %v", err)
	}
	if err = r.Resolve(ctx, uid, sid, one.ID, ""); err == nil {
		t.Fatal("unaudited resolution")
	}
	if err = r.Resolve(ctx, uid, sid, one.ID, "operator confirmed abandon; no retry"); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_audit WHERE request_id=?`, one.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit count %d: %v", audits, err)
	}
	_ = claimTest(t, r, two.ID)
}

func TestMySQLOutboxCASAndCommitBeforeAck(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	one := acceptTest(t, r, testInput(uid, sid, "one", "hello"))
	deliveries, err := r.Outbox(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var d Delivery
	for _, x := range deliveries {
		if x.ID == one.ID {
			d = x
		}
	}
	if d.ID == "" {
		t.Fatal("missing outbox")
	}
	if err = r.Delay(ctx, one.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err = r.DB.QueryRow(`SELECT next_publish_ms FROM execution_outbox WHERE request_id=?`, one.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = r.MarkPublished(ctx, d, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err = r.DB.QueryRow(`SELECT next_publish_ms FROM execution_outbox WHERE request_id=?`, one.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("stale publisher overwrote durable handoff")
	}
	e := claimTest(t, r, one.ID)
	completeTest(t, r, e, "saved")
	duplicate, err := r.Claim(ctx, one.ID, "duplicate", time.Minute)
	if err != nil || duplicate.State != Completed {
		t.Fatalf("duplicate terminal: %+v %v", duplicate, err)
	}
	if err = r.StartCall(ctx, e, testPolicy(), 100); !errors.Is(err, ErrStale) {
		t.Fatalf("duplicate call boundary: %v", err)
	}
	if ok, err := r.Ackable(ctx, one.ID); err != nil || !ok {
		t.Fatalf("committed not ackable: %v %v", ok, err)
	}
	if ok, err := r.Ackable(ctx, uuid.NewString()); ok || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing evidence silently ackable: %v %v", ok, err)
	}
	messages, receipts, err := r.SnapshotHistory(ctx, uid, sid)
	if err != nil || len(messages) != 2 || len(receipts) != 1 || receipts[0].State != Completed {
		t.Fatalf("snapshot inconsistent %v %v %v", messages, receipts, err)
	}
}

func TestMySQLQuotaPersistsAcrossOwnersAndUncertainty(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	sid2 := testSession(t, r, uid)
	e1 := claimTest(t, r, acceptTest(t, r, testInput(uid, sid, "one", "first")).ID)
	e2 := claimTest(t, r, acceptTest(t, r, testInput(uid, sid2, "two", "second")).ID)
	policy := testPolicy()
	policy.User.Concurrency = 1
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, e := range []Evidence{e1, e2} {
		go func(e Evidence) { <-start; errs <- NewRepository(r.DB, r.limits).StartCall(ctx, e, policy, 100) }(e)
	}
	close(start)
	a, b := <-errs, <-errs
	if !((a == nil && errors.Is(b, ErrLimit)) || (b == nil && errors.Is(a, ErrLimit))) {
		t.Fatalf("global quota multiplied: %v %v", a, b)
	}
	got1, err := r.Get(ctx, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	winner, loser := e1, e2
	if !got1.CallStarted {
		winner, loser = e2, e1
	}
	expireLease(t, r, winner.ID)
	if _, err = r.Recover(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err = r.StartCall(ctx, loser, policy, 100); !errors.Is(err, ErrLimit) {
		t.Fatalf("expired worker released unknown upstream quota: %v", err)
	}
	if err = r.Resolve(ctx, uid, winner.SessionID, winner.ID, "abandon unknown without assuming provider stopped"); err != nil {
		t.Fatal(err)
	}
	if err = r.StartCall(ctx, loser, policy, 100); !errors.Is(err, ErrLimit) {
		t.Fatalf("abandon refunded unknown quota: %v", err)
	}
	if err = r.Defer(ctx, loser, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = r.Renew(ctx, loser, time.Minute); !errors.Is(err, ErrStale) {
		t.Fatalf("deferred old owner renewed: %v", err)
	}
}

func TestMySQLAttachmentsSharedPinnedAndDelete(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	a, err := r.Upload(ctx, uid, sid, "note.txt", "text/plain", []byte("shared database object"))
	if err != nil {
		t.Fatal(err)
	}
	other := NewRepository(r.DB, r.limits)
	loaded, err := other.LoadAttachment(ctx, uid, sid, a.ID)
	if err != nil || string(loaded.Data) != "shared database object" {
		t.Fatalf("other host load %+v %v", loaded, err)
	}
	in := testInput(uid, sid, "one", "read attachment")
	in.AttachmentIDs = []int64{a.ID}
	receipt := acceptTest(t, r, in)
	if err = r.DeleteAttachment(ctx, uid, sid, a.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("deleted pinned object %v", err)
	}
	if _, err = r.DB.Exec(`UPDATE execution_attachments SET expires_ms=0 WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = other.LoadAttachment(ctx, uid, sid, a.ID); err != nil {
		t.Fatalf("expiry hid pinned object %v", err)
	}
	if _, err = r.CleanupAttachments(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = other.LoadAttachment(ctx, uid+1, sid, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross tenant attachment %v", err)
	}
	completeTest(t, r, claimTest(t, r, receipt.ID), "read complete")
	if _, err = r.CleanupAttachments(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = other.LoadAttachment(ctx, uid, sid, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired object retained %v", err)
	}
	if err = r.DeleteSession(ctx, uid, sid); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLAcceptTransactionRollbackAndMigration(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	if err := Migrate(ctx, r.DB); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckSchema(ctx); err != nil {
		t.Fatal(err)
	}
	trigger := "execution_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	query := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON execution_outbox FOR EACH ROW BEGIN IF EXISTS(SELECT 1 FROM execution_evidence WHERE id=NEW.request_id AND user_id=%d) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected acceptance rollback'; END IF; END`, trigger, uid)
	if _, err := r.DB.Exec(query); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.DB.Exec(`DROP TRIGGER IF EXISTS ` + trigger) })
	if _, _, err := r.Accept(ctx, testInput(uid, sid, "one", "must rollback")); err == nil {
		t.Fatal("fault injection did not fire")
	}
	var count int
	for _, table := range []string{"messages", "execution_evidence"} {
		if err := r.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE session_id=?`, sid).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial commit in %s: %d %v", table, count, err)
		}
	}
	if _, err := r.DB.Exec(`DROP TRIGGER ` + trigger); err != nil {
		t.Fatal(err)
	}
	one := acceptTest(t, r, testInput(uid, sid, "one", "must rollback"))
	if one.Sequence != 1 {
		t.Fatalf("rollback consumed sequence: %d", one.Sequence)
	}
}

func TestMySQLAdmissionSurvivesSessionDeletion(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	r.limits.AcceptRPM = 1
	ctx := context.Background()
	e := claimTest(t, r, acceptTest(t, r, testInput(uid, sid, "one", "first")).ID)
	completeTest(t, r, e, "done")
	if err := r.DeleteSession(ctx, uid, sid); err != nil {
		t.Fatal(err)
	}
	sid = testSession(t, r, uid)
	if _, _, err := r.Accept(ctx, testInput(uid, sid, "two", "second")); !errors.Is(err, ErrLimit) {
		t.Fatalf("session deletion reset user admission quota: %v", err)
	}
}

func TestMySQLConcurrentAttachmentPinOrDelete(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	a, err := r.Upload(ctx, uid, sid, "race.txt", "text/plain", []byte("pin versus delete"))
	if err != nil {
		t.Fatal(err)
	}
	in := testInput(uid, sid, "one", "attachment race")
	in.AttachmentIDs = []int64{a.ID}
	start := make(chan struct{})
	acceptErr := make(chan error, 1)
	deleteErr := make(chan error, 1)
	go func() { <-start; _, _, err := r.Accept(ctx, in); acceptErr <- err }()
	go func() { <-start; deleteErr <- r.DeleteAttachment(ctx, uid, sid, a.ID) }()
	close(start)
	aerr, derr := <-acceptErr, <-deleteErr
	if !((aerr == nil && errors.Is(derr, ErrBusy)) || (errors.Is(aerr, ErrNotFound) && derr == nil)) {
		t.Fatalf("unsafe pin/delete: accept=%v delete=%v", aerr, derr)
	}
}

func TestMySQLRateAndTokenReservationNotRefunded(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	policy := testPolicy()
	policy.User.RPM = 1
	one := claimTest(t, r, acceptTest(t, r, testInput(uid, sid, "one", "first")).ID)
	if err := r.StartCall(ctx, one, policy, 100); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, one, Completed, "done", ""); err != nil {
		t.Fatal(err)
	}
	two := claimTest(t, r, acceptTest(t, r, testInput(uid, sid, "two", "second")).ID)
	if err := r.StartCall(ctx, two, policy, 100); !errors.Is(err, ErrLimit) {
		t.Fatalf("completion refunded RPM: %v", err)
	}
	policy.User.RPM = 100
	policy.User.TPM = 150
	if err := r.StartCall(ctx, two, policy, 100); !errors.Is(err, ErrLimit) {
		t.Fatalf("completion refunded TPM: %v", err)
	}
	e, err := r.Get(ctx, two.ID)
	if err != nil || e.CallStarted {
		t.Fatalf("quota failure crossed call boundary: %+v %v", e, err)
	}
}
