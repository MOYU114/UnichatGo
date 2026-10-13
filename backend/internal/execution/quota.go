package execution

import (
	"context"
	"fmt"
	"sort"
	"time"
)

func (r *Repository) StartCall(ctx context.Context, e Evidence, policy QuotaPolicy, tokens int64) error {
	return r.reserve(ctx, e, policy, tokens)
}

func (r *Repository) reserve(ctx context.Context, want Evidence, policy QuotaPolicy, tokens int64) error {
	if tokens <= 0 {
		return fmt.Errorf("positive conservative token reservation required")
	}
	tx, e, err := r.locked(ctx, want.ID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkPolicy(ctx, tx, policy); err != nil {
		return err
	}
	now, err := nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if !owns(e, want, now) {
		return ErrStale
	}
	if e.CallStarted {
		return ErrConflict
	}
	type scopeLimit struct {
		scope string
		limit QuotaLimit
	}
	scopes := []scopeLimit{{"platform", policy.Platform}, {fmt.Sprintf("user:%d", e.UserID), policy.User}, {"credential:" + HashPayload([]byte(e.QuotaScope)), policy.Credential}}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].scope < scopes[j].scope })
	for _, scope := range scopes {
		l := scope.limit
		if l.Concurrency <= 0 || l.RPM <= 0 || l.TPM <= 0 {
			return fmt.Errorf("all quota dimensions must be bounded")
		}
		if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO execution_quota_scopes(scope) VALUES(?)`, []byte(scope.scope)); err != nil {
			return err
		}
		var locked string
		if err = tx.QueryRowContext(ctx, `SELECT scope FROM execution_quota_scopes WHERE scope=? FOR UPDATE`, []byte(scope.scope)).Scan(&locked); err != nil {
			return err
		}
	}
	// Scope-lock contention may outlast the lease; validate again after ALL locks.
	now, err = nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if !owns(e, want, now) {
		return ErrStale
	}
	for _, scope := range scopes {
		l := scope.limit
		var concurrency, requests, used int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT request_id) FROM execution_quota_calls WHERE scope=? AND hold_until_ms>? AND request_id<>?`, []byte(scope.scope), now, e.ID).Scan(&concurrency); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(tokens),0) FROM execution_quota_calls WHERE scope=? AND created_ms>?`, []byte(scope.scope), now-60000).Scan(&requests, &used); err != nil {
			return err
		}
		if concurrency >= int64(l.Concurrency) || requests >= int64(l.RPM) || tokens > l.TPM || used > l.TPM-tokens {
			return ErrLimit
		}
	}
	// Permit lifetime is deliberately independent of the worker lease. A dead or
	// partitioned worker does not prove the external service stopped processing.
	grace := max(policy.Grace, 30*time.Second)
	for _, scope := range scopes {
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_quota_calls(scope,request_id,fence,tokens,created_ms,hold_until_ms) VALUES(?,?,?,?,?,?)`, []byte(scope.scope), e.ID, e.Fence, tokens, now, e.DeadlineMS+grace.Milliseconds()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET call_started=TRUE WHERE id=?`, e.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// Defer is safe only before the external-call boundary. A persisted handoff
// permits ACK without leaving the task dependent on an in-memory timer.
func (r *Repository) Defer(ctx context.Context, want Evidence, delay time.Duration) error {
	tx, e, err := r.locked(ctx, want.ID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if !owns(e, want, now) || e.CallStarted {
		return ErrStale
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET state='queued',owner='',fence=fence+1,lease_until_ms=0 WHERE id=?`, e.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_outbox SET generation=generation+1,next_publish_ms=? WHERE request_id=?`, now+max(delay.Milliseconds(), int64(0)), e.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// PruneQuota removes facts only after both their rate window and conservative
// concurrency hold have elapsed; this never resets live quota on Redis loss.
func (r *Repository) PruneQuota(ctx context.Context, limit int) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM execution_quota_calls WHERE hold_until_ms<`+sqlNow+` AND created_ms<`+sqlNow+`-60000 LIMIT ?`, max(1, limit))
	if err == nil {
		_, err = r.DB.ExecContext(ctx, `DELETE FROM execution_admission_events WHERE created_ms<`+sqlNow+`-60000 LIMIT ?`, max(1, limit))
	}
	return err
}
