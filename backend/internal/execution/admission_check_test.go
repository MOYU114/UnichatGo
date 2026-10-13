package execution

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"unichatgo/internal/models"
)

func TestMySQLAcceptanceCheckAtomicReplayAndContext(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	first := acceptTest(t, r, testInput(uid, sid, "first-budget", "previous"))
	completeTest(t, r, claimTest(t, r, first.ID), "answer")
	failed := acceptTest(t, r, testInput(uid, sid, "failed-budget", "excluded"))
	e := claimTest(t, r, failed.ID)
	if err := r.Finish(ctx, e, Failed, "", "internal_failure"); err != nil {
		t.Fatal(err)
	}
	_ = acceptTest(t, r, testInput(uid, sid, "queued-budget", "future"))
	in := testInput(uid, sid, "checked-race", "current")
	var checks atomic.Int64
	check := func(history []models.Message, _ []Attachment) error {
		checks.Add(1)
		if len(history) != 3 || history[0].Content != "previous" || history[1].Content != "answer" || history[2].Content != "current" {
			return errors.New("wrong effective context")
		}
		return nil
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	receipts := make(chan Receipt, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a, _, err := r.AcceptChecked(ctx, in, check)
			receipts <- a
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(receipts)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for a := range receipts {
		if id != "" && id != a.ID {
			t.Fatal("concurrent replay identity changed")
		}
		id = a.ID
	}
	if checks.Load() != 1 {
		t.Fatalf("budget checked %d times for same key", checks.Load())
	}
	_, replay, err := r.AcceptChecked(ctx, in, func([]models.Message, []Attachment) error { return errors.New("must not run on replay") })
	if err != nil || !replay {
		t.Fatalf("new budget invalidated receipt: %v replay=%v", err, replay)
	}
}
