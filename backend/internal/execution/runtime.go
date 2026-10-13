package execution

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const streamKey = "unichatgo:execution:v1"
const consumerGroup = "execution-v1"

// Snapshot is encrypted in SQL; only its request ID is sent to Redis.
type Snapshot struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	Token           string `json:"token"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

type Runtime struct {
	Repo           *Repository
	Redis          *redis.Client
	Provider       Provider
	Decrypt        func(string) (string, error)
	Quota          QuotaPolicy
	Workers        int
	Lease          time.Duration
	Published      atomic.Int64
	Completed      atomic.Int64
	UncertainCount atomic.Int64
	Errors         atomic.Int64
}

func (r *Runtime) Ready(ctx context.Context) error {
	if err := r.Repo.CheckSchema(ctx); err != nil {
		return err
	}
	return r.Redis.Ping(ctx).Err()
}

// Run owns pull capacity and drains already-started work independently of HTTP.
func (r *Runtime) Run(ctx context.Context) {
	if r.Workers <= 0 {
		r.Workers = 4
	}
	if r.Lease <= 0 {
		r.Lease = 30 * time.Second
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.publishLoop(ctx) }()
	for i := 0; i < r.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.consumeLoop(ctx, uuid.NewString()) }()
	}
	wg.Wait()
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (r *Runtime) publishLoop(ctx context.Context) {
	for ctx.Err() == nil {
		// SQL recovery is classification/repair, never normal task claiming.
		if _, err := r.Repo.Recover(ctx, 100); err != nil {
			r.Errors.Add(1)
		}
		if _, err := r.Repo.CleanupAttachments(ctx, 100); err != nil {
			r.Errors.Add(1)
		}
		if err := r.Repo.PruneQuota(ctx, 100); err != nil {
			r.Errors.Add(1)
		}
		if err := r.PublishOnce(ctx); err != nil {
			r.Errors.Add(1)
		}
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func (r *Runtime) PublishOnce(ctx context.Context) error {
	batch, err := r.Repo.Outbox(ctx, 100)
	if err != nil {
		return err
	}
	for _, d := range batch {
		if _, err = r.Redis.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: map[string]any{"request_id": d.ID}}).Result(); err != nil {
			return err
		}
		r.Published.Add(1)
		// Generation CAS cannot erase a newer consumer's deferred handoff.
		if err = r.Repo.MarkPublished(ctx, d, 30*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) consumeLoop(ctx context.Context, owner string) {
	cursor := "0-0"
	for ctx.Err() == nil {
		err := r.Redis.XGroupCreateMkStream(ctx, streamKey, consumerGroup, "0").Err()
		if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		pending, next, err := r.Redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: streamKey, Group: consumerGroup, Consumer: owner, MinIdle: r.Lease, Start: cursor, Count: 1}).Result()
		if err == nil {
			cursor = next
			for _, m := range pending {
				r.deliver(ctx, owner, m)
			}
		}
		if ctx.Err() != nil {
			return
		}
		streams, err := r.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: consumerGroup, Consumer: owner, Streams: []string{streamKey, ">"}, Count: 1, Block: time.Second}).Result()
		if err != nil {
			if !errors.Is(err, redis.Nil) {
				r.Errors.Add(1)
				if !wait(ctx, time.Second) {
					return
				}
			}
			continue
		}
		for _, stream := range streams {
			for _, m := range stream.Messages {
				r.deliver(ctx, owner, m)
			}
		}
	}
}

func (r *Runtime) ack(ctx context.Context, id string) {
	// XDEL only follows XACK; never trim unresolved entries or another group.
	if err := r.Redis.XAck(ctx, streamKey, consumerGroup, id).Err(); err == nil {
		_ = r.Redis.XDel(ctx, streamKey, id).Err()
	} else {
		r.Errors.Add(1)
	}
}

func (r *Runtime) deliver(ctx context.Context, owner string, m redis.XMessage) {
	id, ok := m.Values["request_id"].(string)
	if _, err := uuid.Parse(id); !ok || err != nil {
		if err := r.Repo.Quarantine(ctx, m.ID, "invalid_envelope"); err == nil {
			r.ack(ctx, m.ID)
		}
		return
	}
	ready, err := r.Repo.Ackable(ctx, id)
	if errors.Is(err, ErrNotFound) {
		if err = r.Repo.Quarantine(ctx, m.ID, "missing_evidence"); err == nil {
			r.ack(ctx, m.ID)
		}
		return
	}
	if err != nil {
		r.Errors.Add(1)
		return
	}
	if ready {
		r.ack(ctx, m.ID)
		return
	}
	e, err := r.Repo.Claim(ctx, id, owner, r.Lease)
	if errors.Is(err, ErrNotHead) || errors.Is(err, ErrBlocked) {
		if err = r.Repo.Delay(ctx, id, 2*time.Second); err == nil {
			r.ack(ctx, m.ID)
		}
		return
	}
	if err != nil {
		return
	}
	if e.State != Running {
		if ready, err := r.Repo.Ackable(ctx, id); err == nil && ready {
			r.ack(ctx, m.ID)
		}
		return
	}
	log.Printf("execution request=%s sequence=%d fence=%d event=claimed", e.ID, e.Sequence, e.Fence)
	r.execute(e)
	// The original HTTP and pull contexts are not execution/commit authority.
	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ready, err = r.Repo.Ackable(ackCtx, id); err == nil && ready {
		r.ack(ackCtx, m.ID)
	}
}

func (r *Runtime) execute(e Evidence) {
	clockStarted := time.Now()
	clockCtx, clockCancel := context.WithTimeout(context.Background(), 5*time.Second)
	var sqlTime int64
	err := r.Repo.DB.QueryRowContext(clockCtx, `SELECT `+sqlNow).Scan(&sqlTime)
	clockCancel()
	if err != nil {
		return
	}
	remaining := time.Duration(e.DeadlineMS-sqlTime)*time.Millisecond - time.Since(clockStarted)
	if remaining <= 0 {
		remaining = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	providerCtx, cancelProvider := context.WithCancel(ctx)
	defer cancelProvider()
	heartbeatDone := make(chan struct{})
	defer func() { cancel(); <-heartbeatDone }()
	go func() {
		defer close(heartbeatDone)
		validUntil := clockStarted.Add(time.Duration(e.LeaseUntilMS-sqlTime) * time.Millisecond)
		for wait(ctx, r.Lease/3) {
			started := time.Now()
			if err := r.Repo.Renew(ctx, e, r.Lease); err != nil {
				r.Errors.Add(1)
				if errors.Is(err, ErrStale) || errors.Is(err, ErrNotFound) || !time.Now().Before(validUntil) {
					cancelProvider()
					return
				}
				continue
			}
			validUntil = started.Add(r.Lease)
		}
	}()
	plain, err := r.Decrypt(e.Snapshot)
	var snapshot Snapshot
	if err == nil {
		err = json.Unmarshal([]byte(plain), &snapshot)
	}
	if err != nil {
		r.save(ctx, e, Failed, "", "internal_snapshot")
		return
	}
	history, err := r.Repo.Context(ctx, e)
	if err != nil {
		_ = r.Repo.Defer(ctx, e, time.Second)
		return
	}
	input := CallInput{Provider: snapshot.Provider, Model: snapshot.Model, BaseURL: snapshot.BaseURL, Token: snapshot.Token, Messages: history, MaxOutputTokens: snapshot.MaxOutputTokens}
	for _, id := range e.AttachmentIDs {
		a, err := r.Repo.LoadAttachment(ctx, e.UserID, e.SessionID, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				r.save(ctx, e, Failed, "", "internal_attachment")
			} else {
				_ = r.Repo.Defer(ctx, e, time.Second)
			}
			return
		}
		input.Attachments = append(input.Attachments, a)
	}
	tokens, err := EstimateTokens(input)
	if err != nil {
		r.save(ctx, e, Failed, "", "input_budget_invalid")
		return
	}
	if tokens > r.Quota.Platform.TPM || tokens > r.Quota.User.TPM || tokens > r.Quota.Credential.TPM {
		r.save(ctx, e, Failed, "", "token_budget_exceeded")
		return
	}
	if err = r.Repo.StartCall(ctx, e, r.Quota, tokens); err != nil {
		if errors.Is(err, ErrLimit) {
			_ = r.Repo.Defer(ctx, e, 2*time.Second)
		}
		return
	}
	content, err := r.Provider.Generate(providerCtx, input)
	state, code := Completed, ""
	if err != nil {
		state, code = Uncertain, "upstream_unknown"
		var pe *ProviderError
		if errors.As(err, &pe) {
			code = pe.Code
			if !pe.Uncertain {
				state = Failed
			}
		}
	}
	r.save(ctx, e, state, content, code)
}

func (r *Runtime) save(ctx context.Context, e Evidence, state State, content, code string) {
	// Keep exactly this response in memory across transient DB errors, never regenerate.
	for i := 0; i < 5; i++ {
		err := r.Repo.Finish(ctx, e, state, content, code)
		if err == nil {
			if state == Completed {
				r.Completed.Add(1)
			}
			if state == Uncertain {
				r.UncertainCount.Add(1)
			}
			log.Printf("execution request=%s sequence=%d fence=%d event=committed state=%s code=%s", e.ID, e.Sequence, e.Fence, state, code)
			return
		}
		if errors.Is(err, ErrStale) {
			return
		}
		r.Errors.Add(1)
		if !wait(ctx, time.Duration(1<<i)*100*time.Millisecond) {
			return
		}
	}
}
