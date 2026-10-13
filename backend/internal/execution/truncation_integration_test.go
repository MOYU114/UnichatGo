package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"unichatgo/internal/models"
)

func TestMySQLRuntimeTruncationFailsWithoutBlockingOrRetry(t *testing.T) {
	for _, tc := range []struct{ provider, truncated, completed string }{
		{"openai", `{"choices":[{"finish_reason":"length","message":{"content":"discarded partial answer"}}]}`, `{"choices":[{"finish_reason":"stop","message":{"content":"complete answer"}}]}`},
		{"claude", `{"type":"message","stop_reason":"max_tokens","content":[{"type":"text","text":"discarded partial answer"}]}`, `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"complete answer"}]}`},
		{"gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"discarded partial answer"}]}}]}`, `{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"complete answer"}]}}]}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			repo, uid, sid := mysqlRepository(t)
			ctx := context.Background()
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch calls.Add(1) {
				case 1:
					if strings.Count(string(body), "truncated question") != 1 || strings.Contains(string(body), "next question") {
						t.Errorf("first request has duplicate or future input: %s", body)
					}
					fmt.Fprint(w, tc.truncated)
				case 2:
					if strings.Count(string(body), "next question") != 1 || strings.Contains(string(body), "truncated question") || strings.Contains(string(body), "discarded partial answer") {
						t.Errorf("next request includes failed turn: %s", body)
					}
					fmt.Fprint(w, tc.completed)
				default:
					t.Errorf("unexpected provider retry")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			provider := NewHTTPProvider(time.Second)
			defer provider.Close()
			rt := &Runtime{Repo: repo, Provider: provider, Decrypt: func(s string) (string, error) { return s, nil }, Quota: testPolicy(), Lease: time.Minute}
			snapshot, err := json.Marshal(Snapshot{Provider: tc.provider, Model: "test-model", BaseURL: server.URL, Token: "test-only", MaxOutputTokens: 100})
			if err != nil {
				t.Fatal(err)
			}
			input := testInput(uid, sid, "first", "truncated question")
			input.Provider, input.Model, input.Snapshot = tc.provider, "test-model", string(snapshot)
			first := acceptTest(t, repo, input)
			input.ClientID, input.Content, input.PayloadHash = "second", "next question", HashPayload([]byte("next question"))
			second := acceptTest(t, repo, input)

			rt.execute(claimTest(t, repo, first.ID))
			failed, err := repo.Get(ctx, first.ID)
			if err != nil || failed.State != Failed || failed.ErrorCode != "provider_output_truncated" || failed.AssistantMessageID != 0 || calls.Load() != 1 {
				t.Fatalf("truncation not durably failed: %+v, %v; calls=%d", failed, err, calls.Load())
			}
			messages, receipts, err := repo.SnapshotHistory(ctx, uid, sid)
			if err != nil || len(receipts) != 2 || receipts[0].State != Failed || receipts[0].ErrorCode != "provider_output_truncated" {
				t.Fatalf("truncation not visible in history: %+v, %v", receipts, err)
			}
			for _, message := range messages {
				if message.Role == models.RoleAssistant || strings.Contains(message.Content, "discarded partial answer") {
					t.Fatalf("truncated output persisted as assistant: %+v", message)
				}
			}
			// The same durable gate used by delivery suppresses calls after lost ACK/restart.
			restarted := NewRepository(repo.DB, repo.limits)
			ackable, err := restarted.Ackable(ctx, first.ID)
			if err != nil || !ackable {
				t.Fatalf("failed delivery not safely ackable: %v, %v", ackable, err)
			}
			replayed, err := restarted.Claim(ctx, first.ID, "replacement-worker", time.Minute)
			if err != nil || replayed.State != Failed || calls.Load() != 1 {
				t.Fatalf("terminal delivery became runnable: %+v, %v; calls=%d", replayed, err, calls.Load())
			}
			next := claimTest(t, repo, second.ID)
			history, err := repo.Context(ctx, next)
			if err != nil || len(history) != 1 || history[0].Role != models.RoleUser || history[0].Content != "next question" {
				t.Fatalf("failed turn contaminated subsequent context: %+v, %v", history, err)
			}
			rt.execute(next)
			completed, err := repo.Get(ctx, second.ID)
			if err != nil || completed.State != Completed || completed.AssistantMessageID == 0 || calls.Load() != 2 {
				t.Fatalf("next task did not complete exactly once: %+v, %v; calls=%d", completed, err, calls.Load())
			}
			input.ClientID, input.Content, input.PayloadHash = "third", "new question", HashPayload([]byte("new question"))
			if accepted := acceptTest(t, repo, input); accepted.State != Queued {
				t.Fatalf("new request blocked after terminal truncation: %+v", accepted)
			}
		})
	}
}
