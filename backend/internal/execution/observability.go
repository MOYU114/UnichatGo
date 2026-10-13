package execution

import (
	"context"
	"database/sql"
	"time"
)

// Metrics contains only bounded aggregate labels; no user/key/payload data.
type Metrics struct {
	Queued         int64 `json:"queued"`
	Running        int64 `json:"running"`
	Uncertain      int64 `json:"uncertain"`
	Failed         int64 `json:"failed"`
	OldestQueuedMS int64 `json:"oldest_queued_ms"`
	OutboxDue      int64 `json:"outbox_due"`
	Pending        int64 `json:"pending"`
	Published      int64 `json:"process_published"`
	Completed      int64 `json:"process_completed"`
	Errors         int64 `json:"process_errors"`
	DBOpen         int   `json:"db_open_connections"`
	DBInUse        int   `json:"db_in_use"`
	DBWaitCount    int64 `json:"db_wait_count"`
	DBWaitMS       int64 `json:"db_wait_ms"`
}

func (r *Runtime) Metrics(ctx context.Context) (Metrics, error) {
	var m Metrics
	var age sql.NullInt64
	err := r.Repo.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(state='queued'),0),COALESCE(SUM(state='running'),0),COALESCE(SUM(state='uncertain'),0),COALESCE(SUM(state='failed'),0),MIN(CASE WHEN state='queued' THEN created_ms ELSE NULL END) FROM execution_evidence`).Scan(&m.Queued, &m.Running, &m.Uncertain, &m.Failed, &age)
	if err != nil {
		return m, err
	}
	var now int64
	if err = r.Repo.DB.QueryRowContext(ctx, `SELECT `+sqlNow).Scan(&now); err != nil {
		return m, err
	}
	if age.Valid {
		m.OldestQueuedMS = max(0, now-age.Int64)
	}
	if err = r.Repo.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_outbox WHERE next_publish_ms<=`+sqlNow).Scan(&m.OutboxDue); err != nil {
		return m, err
	}
	groups, err := r.Redis.XInfoGroups(ctx, streamKey).Result()
	if err != nil && err.Error() != "ERR no such key" {
		return m, err
	}
	for _, g := range groups {
		if g.Name == consumerGroup {
			m.Pending = g.Pending
		}
	}
	m.Published = r.Published.Load()
	m.Completed = r.Completed.Load()
	m.Errors = r.Errors.Load()
	stats := r.Repo.DB.Stats()
	m.DBOpen = stats.OpenConnections
	m.DBInUse = stats.InUse
	m.DBWaitCount = stats.WaitCount
	m.DBWaitMS = int64(stats.WaitDuration / time.Millisecond)
	return m, nil
}
