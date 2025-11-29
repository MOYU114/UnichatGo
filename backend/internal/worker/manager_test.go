package worker

import (
	"sync"
	"testing"
	"time"

	"unichatgo/internal/models"
)

func TestWorkerStateCacheOperations(t *testing.T) {
	state := newWorkerState()

	session := &models.Session{ID: 1, Title: "pending"}
	state.setSession(session)
	if got := state.getSession(1); got == nil || got.Title != "pending" {
		t.Fatalf("getSession mismatch: %#v", got)
	}

	state.setHistory(1, []*models.Message{{ID: 10}})
	state.appendHistory(1, &models.Message{ID: 11})
	if hist := state.getHistory(1); len(hist) != 2 || hist[1].ID != 11 {
		t.Fatalf("history not updated: %#v", hist)
	}

	state.setResources(1, &sessionResources{provider: "p", model: "m"})
	if res := state.getResources(1); res == nil || res.provider != "p" {
		t.Fatalf("resources not stored: %#v", res)
	}

	state.markReady(1)
	if !state.isReady(1) {
		t.Fatalf("session should be ready")
	}

	waiter := make(chan workerReturn, 1)
	state.addWaiter(1, waiter)
	state.drainWaiters(1, workerReturn{session: session})
	ret := <-waiter
	if ret.session != session {
		t.Fatalf("drainWaiters returned unexpected session")
	}

	pendingID := int64(-2)
	state.sessions[pendingID] = &models.Session{ID: pendingID, Title: "pending"}
	state.history[pendingID] = []*models.Message{{ID: 20}}
	waiter2 := make(chan workerReturn, 1)
	state.addWaiter(pendingID, waiter2)
	state.promoteSession(pendingID, 2)
	if state.getSession(2) == nil {
		t.Fatalf("session not promoted")
	}
	if _, ok := state.waiters[2]; !ok {
		t.Fatalf("waiters not promoted")
	}

	state.purgeCache(2)
	if state.getSession(2) != nil || state.getResources(2) != nil {
		t.Fatalf("purgeCache failed to clear entries")
	}

	state.reset()
	if len(state.sessions) != 0 || len(state.history) != 0 {
		t.Fatalf("reset did not clear caches")
	}
}

func TestManagerPurgeAndStop(t *testing.T) {
	manager := NewManager(nil)
	state := manager.ensureWorker(42)

	state.setSession(&models.Session{ID: 99, Title: "cached"})
	state.setHistory(99, []*models.Message{{ID: 1}})
	state.setResources(99, &sessionResources{provider: "p", model: "m", token: "t"})
	state.markReady(99)

	manager.Purge(42, 99)
	if !waitFor(t, time.Second, func() bool {
		return state.getSession(99) == nil && state.getResources(99) == nil && !state.isReady(99)
	}) {
		t.Fatalf("purge did not clear cached session")
	}

	manager.Stop(42)
	if !waitFor(t, time.Second, func() bool {
		return manager.getWorker(42) == nil
	}) {
		t.Fatalf("worker still present after stop")
	}

	// Ensure calling Purge after Stop is a no-op.
	manager.Purge(42, 99)
}

func TestWorkerStateConcurrentWaiters(t *testing.T) {
	state := newWorkerState()
	session := &models.Session{ID: 7}

	const waiters = 32
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(waiters)
		for i := 0; i < waiters; i++ {
			go func() {
				ch := make(chan workerReturn, 1)
				state.addWaiter(1, ch)
				ret := <-ch
				if ret.session != session {
					t.Errorf("unexpected session: %#v", ret.session)
				}
				wg.Done()
			}()
		}
		wg.Wait()
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	state.drainWaiters(1, workerReturn{session: session})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for waiters to drain")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
