# Independent design review: execution protocol

Date: 2026-10-12. Reviewer: `design_review` sub-agent; not the author of ADR-004's initial protocol. Scope: the complete untracked ADR-004 plus accepted ADR-001/002/003, roadmap, original issue and relevant legacy storage/API code. This is a static design review, not evidence that implementation or fault tests have passed.

## Findings and disposition

| Severity | Original location | Trigger and impact | Required correction / disposition |
| --- | --- | --- | --- |
| P1 | ADR-004, quota paragraph (original line 26) | Reserving quota separately from call-start or expiring permits solely with execution lease can admit an extra call while an old upstream call remains active; Redis rebuild then appears safe but exceeds configured limits. | Couple sorted-scope reservation with `call_started` in one SQL transaction; bind permit to request/fence, reserve through immutable deadline plus grace independently of the worker lease (or atomically renew future shorter permits). Added to ADR. Physical upstream cancellation remains unprovable without provider cooperation. |
| P1 | ADR-004, attachments/delete (original line 39), cascade (line 10) | Session/user deletion can cascade away active evidence and pinned attachments after a call starts, destroying the recovery authority and permitting early quota release. A common lock order alone is insufficient. | Reject delete with queued/running/uncertain evidence; retain quota quarantine independently of parent cascade after explicit abandon. Added to ADR. |
| P1 | ADR-004, migration/context (original lines 12, 19) | Interpreting "legacy context read only before first asynchronous acceptance" literally drops pre-migration conversation context on later turns; deriving legacy by absent evidence risks admitting unrelated new messages. | Persist immutable legacy message-ID cutoff once; include this prefix on every context assembly, followed by completed prior async pairs. Added to ADR. |
| P2 | ADR-004, outbox (original lines 11, 17, 24) | Publisher reads due row, XADD succeeds, consumer durably delays and ACKs, then stale publish-marking overwrites the delay. This loses intended backoff and can create retry storms or excessive delay. | Monotonic outbox generation and CAS marking; deferral increments generation. Durable row remains until terminal/uncertain responsibility transfer. Added to ADR. |
| P2 | ADR-004, identity (original lines 9, 16) | Hashing re-resolved defaults/credentials/implicit attachment sets can turn same-key replay into conflict after config rotation or file expiry, even though prior acceptance is valid. | Versioned canonical client-payload identity; lookup and comparison precede mutable resolution. Freeze resolved semantics separately; explicit attachment list only. Added to ADR. |
| P2 | ADR-004, abandon/timeout (original lines 25-28) | No terminal state, audit-retention or permit-release rule was stated for abandonment; deadline expiry could leave queued tasks permanently pending. | Durable failed `uncertain_abandoned` plus retained audit, fence advance after lease expiry, no early quota refund; deadlines classify pre-call failure versus post-call uncertain. Added to ADR. |

## Re-review

Static re-review of the edited ADR finds the six identified design gaps addressed by explicit invariants. This permits implementing those details; it does not approve a claim that M1-M6 are complete. Implementation must still prove the corresponding behavior with real MySQL locks/transactions, Redis loss/reclaim and controlled provider faults. SQL-backed quota is a valid simplification of the recovery barrier only while those durable ownership and quarantine rules remain enforced.

## Required implementation evidence

- Barrier tests for stale publisher marking versus durable deferral and for completion versus deletion.
- Fault tests for quota/call-boundary rollback, lease expiry with an upstream request blocked, stale renew/release and Redis state loss.
- Context tests covering two async turns after a legacy prefix, excluding future/failed messages.
- Idempotency replay after credential/default rotation and attachment cleanup; no dependency lookup before existing-key return.
- Abandonment ownership/audit tests and retained quota after subsequent session/user deletion.

## Provider subtask ownership

After this independent design review, the same agent is assigned only `backend/internal/execution/provider.go` and `provider_test.go`. The parent agent must independently review those implementation files; this design review must not be represented as independent review of that agent's own provider implementation. Non-streaming raw HTTP with explicit completion markers is the chosen simple boundary; unsupported document/tool paths remain feature-parity blockers until implemented or explicitly scoped out.
