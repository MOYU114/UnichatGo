package execution

import (
	"context"
	"errors"
	"testing"
)

// A shared start barrier allows either lock winner; assertions check the
// persisted outcome, never infer mutual exclusion from elapsed wall time.
func TestMySQLAttachmentAcceptanceDeleteRace(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	for round := range 12 {
		a, err := r.Upload(ctx, uid, sid, "race.txt", "text/plain", []byte("immutable bytes"))
		if err != nil {
			t.Fatal(err)
		}
		in := testInput(uid, sid, "attachment-delete-"+string(rune('a'+round)), "read")
		in.AttachmentIDs = []int64{a.ID}
		start := make(chan struct{})
		accepted := make(chan error, 1)
		deleted := make(chan error, 1)
		go func() { <-start; _, _, err := NewRepository(r.DB, r.limits).Accept(ctx, in); accepted <- err }()
		go func() { <-start; deleted <- NewRepository(r.DB, r.limits).DeleteAttachment(ctx, uid, sid, a.ID) }()
		close(start)
		acceptErr, deleteErr := <-accepted, <-deleted
		switch {
		case acceptErr == nil:
			if !errors.Is(deleteErr, ErrBusy) {
				t.Fatalf("accepted attachment deleted: %v", deleteErr)
			}
			loaded, err := r.LoadAttachment(ctx, uid, sid, a.ID)
			if err != nil || string(loaded.Data) != "immutable bytes" {
				t.Fatalf("accepted bytes unavailable: %v", err)
			}
			receipt, err := r.Lookup(ctx, uid, sid, in.ClientID, in.PayloadHash)
			if err != nil {
				t.Fatal(err)
			}
			var pins int
			if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_attachment_pins WHERE request_id=? AND attachment_id=?`, receipt.ID, a.ID).Scan(&pins); err != nil || pins != 1 {
				t.Fatalf("missing pin: %d %v", pins, err)
			}
		case deleteErr == nil:
			if !errors.Is(acceptErr, ErrNotFound) {
				t.Fatalf("deleted input accepted: %v", acceptErr)
			}
			var messages int
			if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_evidence WHERE session_id=? AND client_id=?`, sid, []byte(in.ClientID)).Scan(&messages); err != nil || messages != 0 {
				t.Fatalf("partial acceptance: %d %v", messages, err)
			}
		default:
			t.Fatalf("neither serializable winner: accept=%v delete=%v", acceptErr, deleteErr)
		}
	}
}

func TestMySQLExpiredAttachmentAcceptanceCleanupRace(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	a, err := r.Upload(ctx, uid, sid, "expired.txt", "text/plain", []byte("expired"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.DB.Exec(`UPDATE execution_attachments SET expires_ms=0 WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	in := testInput(uid, sid, "expired-cleanup", "must not accept")
	in.AttachmentIDs = []int64{a.ID}
	start := make(chan struct{})
	accepted := make(chan error, 1)
	cleaned := make(chan error, 1)
	go func() { <-start; _, _, err := NewRepository(r.DB, r.limits).Accept(ctx, in); accepted <- err }()
	go func() { <-start; _, err := NewRepository(r.DB, r.limits).CleanupAttachments(ctx, 1000); cleaned <- err }()
	close(start)
	if err = <-accepted; !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired input accepted: %v", err)
	}
	if err = <-cleaned; err != nil {
		t.Fatal(err)
	}
	if _, err = r.LoadAttachment(ctx, uid, sid, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired object still visible: %v", err)
	}
	var count int
	if err = r.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`, sid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback failed: messages=%d %v", count, err)
	}
}

func TestMySQLPinnedAttachmentCompletionCleanupRace(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	a, err := r.Upload(ctx, uid, sid, "pinned.txt", "text/plain", []byte("pinned"))
	if err != nil {
		t.Fatal(err)
	}
	in := testInput(uid, sid, "pinned-cleanup", "read")
	in.AttachmentIDs = []int64{a.ID}
	receipt := acceptTest(t, r, in)
	e := claimTest(t, r, receipt.ID)
	if err = r.StartCall(ctx, e, testPolicy(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err = r.DB.Exec(`UPDATE execution_attachments SET expires_ms=0 WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	finished := make(chan error, 1)
	cleaned := make(chan error, 1)
	go func() { <-start; finished <- NewRepository(r.DB, r.limits).Finish(ctx, e, Completed, "done", "") }()
	go func() { <-start; _, err := NewRepository(r.DB, r.limits).CleanupAttachments(ctx, 1000); cleaned <- err }()
	close(start)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	if err = <-cleaned; err != nil {
		t.Fatal(err)
	}
	// If cleanup scanned before completion, a second idempotent pass removes it.
	if _, err = r.CleanupAttachments(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err = r.LoadAttachment(ctx, uid, sid, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpinned expired object retained: %v", err)
	}
	got, err := r.Get(ctx, receipt.ID)
	if err != nil || got.State != Completed {
		t.Fatalf("completion lost: %+v %v", got, err)
	}
}
