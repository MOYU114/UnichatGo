package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"unichatgo/internal/models"
)

type forbidBudgetProvider struct{ t *testing.T }

func (p forbidBudgetProvider) Generate(context.Context, CallInput) (string, error) {
	p.t.Error("queued context growth exceeded budget but called provider")
	return "", errors.New("must not call")
}

func TestMySQLWorkerStillRejectsQueuedContextBudgetGrowth(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	first := acceptTest(t, r, testInput(uid, sid, "grows", "previous question"))
	snapshot, _ := json.Marshal(Snapshot{Provider: "openai", Model: "mock", Token: "test-only", MaxOutputTokens: DefaultMaxOutputTokens})
	in := testInput(uid, sid, "waits", "current question")
	in.Snapshot = string(snapshot)
	checked := false
	second, _, err := r.AcceptChecked(ctx, in, func(history []models.Message, _ []Attachment) error {
		checked = true
		if len(history) != 1 || history[0].Content != in.Content {
			return errors.New("queued input contaminated preflight")
		}
		n, err := EstimateTokens(CallInput{Provider: "openai", Model: "mock", Messages: history})
		if err != nil {
			return err
		}
		if n > 200000 {
			return errors.New("short queued input exceeded default")
		}
		return nil
	})
	if err != nil || !checked {
		t.Fatalf("safe known input not accepted: %v", err)
	}
	// The earlier reply did not exist at acceptance. Completing it changes the
	// later effective context without making preflight a quota reservation.
	completeTest(t, r, claimTest(t, r, first.ID), strings.Repeat("x", 200000))
	limit := QuotaLimit{Concurrency: 4, RPM: 60, TPM: 200000}
	rt := &Runtime{Repo: r, Provider: forbidBudgetProvider{t}, Decrypt: func(s string) (string, error) { return s, nil }, Lease: time.Minute, Quota: QuotaPolicy{Platform: limit, Credential: limit, User: limit}}
	rt.execute(claimTest(t, r, second.ID))
	e, err := r.Get(ctx, second.ID)
	if err != nil || e.State != Failed || e.ErrorCode != "token_budget_exceeded" || e.CallStarted {
		t.Fatalf("queued growth not safely failed: %+v err=%v", e, err)
	}
	var reservations int
	if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_quota_calls WHERE request_id=?`, e.ID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("overbudget call reserved quota: %d %v", reservations, err)
	}
	encoded, _ := json.Marshal(e.Receipt)
	if !bytes.Contains(encoded, []byte("token_budget_exceeded")) {
		t.Fatal("failure not observable")
	}
}
