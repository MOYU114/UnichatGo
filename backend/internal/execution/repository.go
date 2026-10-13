package execution

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"unichatgo/internal/models"
)

const sqlNow = `CAST(UNIX_TIMESTAMP(NOW(3))*1000 AS SIGNED)`
const evidenceColumns = `id,user_message_id,COALESCE(assistant_message_id,0),sequence,state,error_code,user_id,session_id,client_id,payload_hash,provider,model,snapshot,quota_scope,owner,fence,lease_until_ms,deadline_ms,call_started,attachment_ids`

type scanner interface{ Scan(...any) error }

func scanEvidence(s scanner) (Evidence, error) {
	var e Evidence
	var attachments string
	err := s.Scan(&e.ID, &e.UserMessageID, &e.AssistantMessageID, &e.Sequence, &e.State, &e.ErrorCode, &e.UserID, &e.SessionID, &e.ClientID, &e.PayloadHash, &e.Provider, &e.Model, &e.Snapshot, &e.QuotaScope, &e.Owner, &e.Fence, &e.LeaseUntilMS, &e.DeadlineMS, &e.CallStarted, &attachments)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal([]byte(attachments), &e.AttachmentIDs)
	}
	return e, err
}

func (r *Repository) begin(ctx context.Context) (*sql.Tx, error) {
	return r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}
func lockOwned(ctx context.Context, tx *sql.Tx, userID, sessionID int64) error {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id=? AND user_id=? FOR UPDATE`, sessionID, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}
func nowMS(ctx context.Context, tx *sql.Tx) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT `+sqlNow).Scan(&n)
	return n, err
}
func (r *Repository) locked(ctx context.Context, id string) (*sql.Tx, Evidence, error) {
	// Read identity before taking the global lock order. Deletion is rechecked under locks.
	e, err := r.Get(ctx, id)
	if err != nil {
		return nil, e, err
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, e, err
	}
	if err = lockOwned(ctx, tx, e.UserID, e.SessionID); err == nil {
		e, err = scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE id=? FOR UPDATE`, id))
	}
	if err != nil {
		tx.Rollback()
		return nil, e, err
	}
	return tx, e, nil
}
func (r *Repository) Get(ctx context.Context, id string) (Evidence, error) {
	return scanEvidence(r.DB.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE id=?`, id))
}

func (r *Repository) Lookup(ctx context.Context, userID, sessionID int64, clientID, hash string) (Receipt, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return Receipt{}, err
	}
	e, err := scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE user_id=? AND session_id=? AND client_id=?`, userID, sessionID, []byte(clientID)))
	if err != nil {
		return Receipt{}, err
	}
	if e.PayloadHash != hash {
		return Receipt{}, ErrConflict
	}
	return e.Receipt, nil
}

func (r *Repository) Accept(ctx context.Context, in Input) (Receipt, bool, error) {
	return r.AcceptChecked(ctx, in, nil)
}

// AcceptanceCheck runs only local validation while the owned session is locked.
// Inputs match current effective history plus this request once; future queued
// replies are unknowable, so workers must still enforce the final call budget.
type AcceptanceCheck func([]models.Message, []Attachment) error

func (r *Repository) AcceptChecked(ctx context.Context, in Input, check AcceptanceCheck) (Receipt, bool, error) {
	if len(in.ClientID) == 0 || len(in.ClientID) > 128 || len(in.PayloadHash) != 64 || len(in.Content) > 1<<20 || len(in.QuotaScope) > 255 || in.Snapshot == "" {
		return Receipt{}, false, fmt.Errorf("invalid execution input")
	}
	if _, err := hex.DecodeString(in.PayloadHash); err != nil {
		return Receipt{}, false, fmt.Errorf("invalid payload hash")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, in.UserID, in.SessionID); err != nil {
		return Receipt{}, false, err
	}
	e, err := scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE user_id=? AND session_id=? AND client_id=?`, in.UserID, in.SessionID, []byte(in.ClientID)))
	if err == nil {
		if e.PayloadHash != in.PayloadHash {
			return Receipt{}, false, ErrConflict
		}
		return e.Receipt, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Receipt{}, false, err
	}
	if err = r.checkAdmission(ctx, tx); err != nil {
		return Receipt{}, false, err
	}
	var uncertain, userCount, sessionCount, recent int
	now, err := nowMS(ctx, tx)
	if err != nil {
		return Receipt{}, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(state IN ('queued','running','uncertain')),0),COALESCE(SUM(session_id=? AND state IN ('queued','running','uncertain')),0),COALESCE(SUM(session_id=? AND state='uncertain'),0) FROM execution_evidence WHERE user_id=?`, in.SessionID, in.SessionID, in.UserID).Scan(&userCount, &sessionCount, &uncertain)
	if err != nil {
		return Receipt{}, false, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_admission_events WHERE user_id=? AND created_ms>?`, in.UserID, now-60000).Scan(&recent); err != nil {
		return Receipt{}, false, err
	}
	if uncertain > 0 {
		return Receipt{}, false, ErrBlocked
	}
	if userCount >= r.limits.UserQueue || sessionCount >= r.limits.SessionQueue || recent >= r.limits.AcceptRPM {
		return Receipt{}, false, ErrLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT IGNORE INTO execution_sessions(session_id,legacy_max_message_id) SELECT ?,COALESCE(MAX(id),0) FROM messages WHERE session_id=?`, in.SessionID, in.SessionID)
	if err != nil {
		return Receipt{}, false, err
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, `SELECT next_sequence FROM execution_sessions WHERE session_id=? FOR UPDATE`, in.SessionID).Scan(&seq); err != nil {
		return Receipt{}, false, err
	}
	seen := map[int64]bool{}
	var frozenAttachments []Attachment
	for _, id := range in.AttachmentIDs {
		if seen[id] {
			return Receipt{}, false, fmt.Errorf("duplicate attachment")
		}
		seen[id] = true
		var found int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM execution_attachments WHERE id=? AND user_id=? AND session_id=? AND expires_ms>? FOR UPDATE`, id, in.UserID, in.SessionID, now).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = ErrNotFound
			}
			return Receipt{}, false, err
		}
		if check != nil {
			a, err := scanAttachment(tx.QueryRowContext(ctx, `SELECT `+attachmentColumns+`,data FROM execution_attachments WHERE id=?`, id), true)
			if err != nil {
				return Receipt{}, false, err
			}
			frozenAttachments = append(frozenAttachments, a)
		}
	}
	if check != nil {
		history, err := effectiveContext(ctx, tx, in.SessionID, seq, 0)
		if err != nil {
			return Receipt{}, false, err
		}
		history = append(history, models.Message{UserID: in.UserID, SessionID: in.SessionID, Role: models.RoleUser, Content: in.Content})
		if err = check(history, frozenAttachments); err != nil {
			return Receipt{}, false, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages(user_id,session_id,role,content,created_at) VALUES(?,?,'user',?,NOW(3))`, in.UserID, in.SessionID, in.Content)
	if err != nil {
		return Receipt{}, false, err
	}
	messageID, err := res.LastInsertId()
	if err != nil {
		return Receipt{}, false, err
	}
	id := uuid.NewString()
	attachments, _ := json.Marshal(in.AttachmentIDs)
	_, err = tx.ExecContext(ctx, `INSERT INTO execution_evidence(id,user_id,session_id,client_id,payload_hash,user_message_id,sequence,provider,model,snapshot,quota_scope,attachment_ids,state,created_ms,deadline_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'queued',?,?)`, id, in.UserID, in.SessionID, []byte(in.ClientID), in.PayloadHash, messageID, seq, in.Provider, in.Model, in.Snapshot, []byte(in.QuotaScope), string(attachments), now, now+r.limits.TaskTimeout.Milliseconds())
	if err != nil {
		return Receipt{}, false, err
	}
	for _, a := range in.AttachmentIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_attachment_pins(request_id,attachment_id) VALUES(?,?)`, id, a); err != nil {
			return Receipt{}, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO execution_outbox(request_id,next_publish_ms) VALUES(?,?)`, id, now); err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO execution_admission_events(user_id,created_ms) VALUES(?,?)`, in.UserID, now); err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_sessions SET next_sequence=next_sequence+1 WHERE session_id=?`, in.SessionID); err != nil {
		return Receipt{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at=NOW(3) WHERE id=?`, in.SessionID); err != nil {
		return Receipt{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{ID: id, ClientID: in.ClientID, SessionID: in.SessionID, UserMessageID: messageID, Sequence: seq, State: Queued}, false, nil
}

func (r *Repository) Claim(ctx context.Context, id, owner string, lease time.Duration) (Evidence, error) {
	if owner == "" || len(owner) > 128 || lease <= 0 {
		return Evidence{}, fmt.Errorf("invalid lease")
	}
	tx, e, err := r.locked(ctx, id)
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	now, err := nowMS(ctx, tx)
	if err != nil {
		return e, err
	}
	if e.State == Completed || e.State == Failed || e.State == Uncertain {
		return e, nil
	}
	if e.State == Running && e.LeaseUntilMS > now {
		return e, ErrBusy
	}
	if e.CallStarted {
		if err = terminal(ctx, tx, e, Uncertain, "", "owner_lost", false); err != nil {
			return e, err
		}
		e.State = Uncertain
		e.ErrorCode = "owner_lost"
		return e, tx.Commit()
	}
	if e.DeadlineMS <= now {
		if err = terminal(ctx, tx, e, Failed, "", "deadline_exceeded", true); err != nil {
			return e, err
		}
		e.State = Failed
		e.ErrorCode = "deadline_exceeded"
		return e, tx.Commit()
	}
	var earlier int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_evidence WHERE session_id=? AND sequence<? AND state IN ('queued','running','uncertain')`, e.SessionID, e.Sequence).Scan(&earlier); err != nil {
		return e, err
	}
	if earlier > 0 {
		return e, ErrNotHead
	}
	e.Owner = owner
	e.Fence++
	e.State = Running
	e.LeaseUntilMS = min(now+lease.Milliseconds(), e.DeadlineMS)
	_, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET owner=?,fence=?,state='running',lease_until_ms=? WHERE id=?`, []byte(owner), e.Fence, e.LeaseUntilMS, e.ID)
	if err != nil {
		return e, err
	}
	return e, tx.Commit()
}

func owns(e, want Evidence, now int64) bool {
	return e.State == Running && e.Owner == want.Owner && e.Fence == want.Fence && e.LeaseUntilMS > now && e.DeadlineMS > now
}
func (r *Repository) Renew(ctx context.Context, want Evidence, lease time.Duration) error {
	tx, e, err := r.locked(ctx, want.ID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if lease <= 0 || !owns(e, want, now) {
		return ErrStale
	}
	_, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET lease_until_ms=? WHERE id=?`, min(now+lease.Milliseconds(), e.DeadlineMS), e.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func terminal(ctx context.Context, tx *sql.Tx, e Evidence, state State, content, code string, release bool) error {
	var assistant any
	if state == Completed {
		res, err := tx.ExecContext(ctx, `INSERT INTO messages(user_id,session_id,role,content,created_at) VALUES(?,?,'assistant',?,NOW(3))`, e.UserID, e.SessionID, content)
		if err != nil {
			return err
		}
		assistant, err = res.LastInsertId()
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_evidence SET state=?,error_code=?,assistant_message_id=? WHERE id=?`, state, code, assistant, e.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_outbox WHERE request_id=?`, e.ID); err != nil {
		return err
	}
	if state != Uncertain {
		if _, err := tx.ExecContext(ctx, `DELETE FROM execution_attachment_pins WHERE request_id=?`, e.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE execution_sessions SET completed_sequence=GREATEST(completed_sequence,?) WHERE session_id=?`, e.Sequence, e.SessionID); err != nil {
			return err
		}
	}
	if release {
		if _, err := tx.ExecContext(ctx, `UPDATE execution_quota_calls SET hold_until_ms=`+sqlNow+` WHERE request_id=? AND fence=?`, e.ID, e.Fence); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=NOW(3) WHERE id=?`, e.SessionID)
	return err
}

func (r *Repository) Finish(ctx context.Context, want Evidence, state State, content, code string) error {
	if state != Completed && state != Failed && state != Uncertain {
		return fmt.Errorf("invalid terminal state")
	}
	if len(code) > 100 || len(content) > 8<<20 {
		return fmt.Errorf("invalid result size")
	}
	tx, e, err := r.locked(ctx, want.ID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if e.State == state && e.Owner == want.Owner && e.Fence == want.Fence {
		return nil
	}
	now, err := nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if !owns(e, want, now) {
		return ErrStale
	}
	if state == Completed && !e.CallStarted {
		return fmt.Errorf("completion before call boundary")
	}
	if err = terminal(ctx, tx, e, state, content, code, state != Uncertain); err != nil {
		return err
	}
	return tx.Commit()
}

// Delay hands responsibility to SQL before a consumer ACK. Generation invalidates
// a publisher that selected this row before the handoff.
func (r *Repository) Delay(ctx context.Context, id string, delay time.Duration) error {
	tx, e, err := r.locked(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if e.State == Completed || e.State == Failed || e.State == Uncertain {
		return nil
	}
	if e.State != Queued {
		return ErrBusy
	}
	_, err = tx.ExecContext(ctx, `UPDATE execution_outbox SET generation=generation+1,next_publish_ms=`+sqlNow+`+? WHERE request_id=?`, max(delay.Milliseconds(), int64(0)), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) Recover(ctx context.Context, limit int) (int, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT id FROM execution_evidence WHERE (state='running' AND lease_until_ms<=`+sqlNow+`) OR (state='queued' AND deadline_ms<=`+sqlNow+`) ORDER BY deadline_ms LIMIT ?`, max(1, limit))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		changed, err := r.recoverOne(ctx, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}
func (r *Repository) recoverOne(ctx context.Context, id string) (bool, error) {
	tx, e, err := r.locked(ctx, id)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now, err := nowMS(ctx, tx)
	if err != nil {
		return false, err
	}
	if e.State == Running && e.LeaseUntilMS <= now {
		if e.CallStarted {
			err = terminal(ctx, tx, e, Uncertain, "", "owner_lost", false)
		} else if e.DeadlineMS <= now {
			err = terminal(ctx, tx, e, Failed, "", "deadline_exceeded", true)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET state='queued',owner='',fence=fence+1,lease_until_ms=0 WHERE id=?`, id)
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE execution_outbox SET generation=generation+1,next_publish_ms=? WHERE request_id=?`, now, id)
			}
		}
	} else if e.State == Queued && e.DeadlineMS <= now {
		err = terminal(ctx, tx, e, Failed, "", "deadline_exceeded", true)
	} else {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (r *Repository) Outbox(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT request_id,generation FROM execution_outbox WHERE next_publish_ms<=`+sqlNow+` ORDER BY next_publish_ms LIMIT ?`, max(1, limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err = rows.Scan(&d.ID, &d.Generation); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (r *Repository) MarkPublished(ctx context.Context, d Delivery, redeliver time.Duration) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE execution_outbox SET generation=generation+1,publish_count=publish_count+1,next_publish_ms=`+sqlNow+`+? WHERE request_id=? AND generation=?`, max(redeliver.Milliseconds(), int64(100)), d.ID, d.Generation)
	return err
}
func (r *Repository) Ackable(ctx context.Context, id string) (bool, error) {
	e, err := r.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return e.State == Completed || e.State == Failed || e.State == Uncertain, nil
}

func (r *Repository) Resolve(ctx context.Context, userID, sessionID int64, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 {
		return fmt.Errorf("audit reason required (maximum 1000 bytes)")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return err
	}
	e, err := scanEvidence(tx.QueryRowContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE id=? AND user_id=? AND session_id=? FOR UPDATE`, id, userID, sessionID))
	if err != nil {
		return err
	}
	now, err := nowMS(ctx, tx)
	if err != nil {
		return err
	}
	if e.State != Uncertain {
		return ErrConflict
	}
	if e.LeaseUntilMS > now {
		return ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO execution_audit(request_id,actor_id,old_fence,action,reason,created_ms) VALUES(?,?,?,'abandon',?,?)`, id, userID, e.Fence, reason, now); err != nil {
		return err
	}
	if err = terminal(ctx, tx, e, Failed, "", "abandoned_uncertain", false); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_evidence SET fence=fence+1,owner='' WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) History(ctx context.Context, userID, sessionID int64) ([]Receipt, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE user_id=? AND session_id=? ORDER BY sequence`, userID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e.Receipt)
	}
	return out, rows.Err()
}

func (r *Repository) Context(ctx context.Context, want Evidence) ([]models.Message, error) {
	tx, e, err := r.locked(ctx, want.ID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := nowMS(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !owns(e, want, now) {
		return nil, ErrStale
	}
	return effectiveContext(ctx, tx, e.SessionID, e.Sequence, e.UserMessageID)
}

func effectiveContext(ctx context.Context, tx *sql.Tx, sessionID, sequence, currentMessageID int64) ([]models.Message, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.user_id,m.session_id,m.role,m.content,m.created_at FROM messages m LEFT JOIN execution_evidence x ON m.id=x.user_message_id OR m.id=x.assistant_message_id WHERE m.session_id=? AND (m.id<=(SELECT legacy_max_message_id FROM execution_sessions WHERE session_id=?) OR m.id=? OR (x.session_id=? AND x.sequence<? AND x.state='completed')) ORDER BY CASE WHEN x.id IS NULL THEN 0 ELSE 1 END,CASE WHEN x.id IS NULL THEN m.id ELSE x.sequence END,CASE WHEN m.role='user' THEN 0 ELSE 1 END`, sessionID, sessionID, currentMessageID, sessionID, sequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Message
	for rows.Next() {
		var m models.Message
		if err = rows.Scan(&m.ID, &m.UserID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) DeleteSession(ctx context.Context, userID, sessionID int64) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_evidence WHERE session_id=? AND state IN ('queued','running','uncertain')`, sessionID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrBusy
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE id=? AND user_id=?`, sessionID, userID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func HashPayload(canonical []byte) string {
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:])
}

// SnapshotHistory serializes with acceptance/completion so request status and
// messages belong to the same committed view, even under READ COMMITTED.
func (r *Repository) SnapshotHistory(ctx context.Context, userID, sessionID int64) ([]models.Message, []Receipt, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return nil, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+evidenceColumns+` FROM execution_evidence WHERE user_id=? AND session_id=? ORDER BY sequence`, userID, sessionID)
	if err != nil {
		return nil, nil, err
	}
	receipts := []Receipt{}
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		receipts = append(receipts, e.Receipt)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT m.id,m.user_id,m.session_id,m.role,m.content,m.created_at FROM messages m LEFT JOIN execution_evidence x ON m.id=x.user_message_id OR m.id=x.assistant_message_id WHERE m.session_id=? ORDER BY CASE WHEN x.id IS NULL THEN 0 ELSE 1 END,CASE WHEN x.id IS NULL THEN m.id ELSE x.sequence END,CASE WHEN m.role='user' THEN 0 ELSE 1 END`, sessionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	messages := []models.Message{}
	for rows.Next() {
		var m models.Message
		if err = rows.Scan(&m.ID, &m.UserID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, nil, err
		}
		messages = append(messages, m)
	}
	return messages, receipts, rows.Err()
}
