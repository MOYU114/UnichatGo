package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const MaxAttachmentBytes = 8 << 20
const MaxUserAttachmentBytes = 64 << 20
const attachmentColumns = `id,user_id,session_id,name,mime,size,sha256`

func scanAttachment(s scanner, withData bool) (Attachment, error) {
	var a Attachment
	args := []any{&a.ID, &a.UserID, &a.SessionID, &a.Name, &a.MIME, &a.Size, &a.SHA256}
	if withData {
		args = append(args, &a.Data)
	}
	err := s.Scan(args...)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func (r *Repository) Upload(ctx context.Context, userID, sessionID int64, name, mime string, data []byte) (Attachment, error) {
	if len(data) == 0 || len(data) > MaxAttachmentBytes || len(name) == 0 || len(name) > 255 || len(mime) > 255 || strings.ContainsAny(name, "\x00\r\n") {
		return Attachment{}, fmt.Errorf("invalid attachment")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return Attachment{}, err
	}
	var used int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM execution_attachments WHERE user_id=?`, userID).Scan(&used); err != nil {
		return Attachment{}, err
	}
	if used+int64(len(data)) > MaxUserAttachmentBytes {
		return Attachment{}, ErrLimit
	}
	now, err := nowMS(ctx, tx)
	if err != nil {
		return Attachment{}, err
	}
	a := Attachment{UserID: userID, SessionID: sessionID, Name: name, MIME: mime, Size: int64(len(data)), SHA256: HashPayload(data)}
	res, err := tx.ExecContext(ctx, `INSERT INTO execution_attachments(user_id,session_id,name,mime,size,sha256,data,created_ms,expires_ms) VALUES(?,?,?,?,?,?,?,?,?)`, userID, sessionID, name, mime, a.Size, a.SHA256, data, now, now+86400000)
	if err != nil {
		return Attachment{}, err
	}
	a.ID, err = res.LastInsertId()
	if err != nil {
		return Attachment{}, err
	}
	return a, tx.Commit()
}

func (r *Repository) LoadAttachment(ctx context.Context, userID, sessionID, id int64) (Attachment, error) {
	return scanAttachment(r.DB.QueryRowContext(ctx, `SELECT `+attachmentColumns+`,data FROM execution_attachments a WHERE a.id=? AND a.user_id=? AND a.session_id=? AND (a.expires_ms>`+sqlNow+` OR EXISTS(SELECT 1 FROM execution_attachment_pins p WHERE p.attachment_id=a.id))`, id, userID, sessionID), true)
}
func (r *Repository) ListAttachments(ctx context.Context, userID, sessionID int64) ([]Attachment, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+attachmentColumns+` FROM execution_attachments a WHERE user_id=? AND session_id=? AND (expires_ms>`+sqlNow+` OR EXISTS(SELECT 1 FROM execution_attachment_pins p WHERE p.attachment_id=a.id)) ORDER BY id`, userID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (r *Repository) DeleteAttachment(ctx context.Context, userID, sessionID, id int64) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, userID, sessionID); err != nil {
		return err
	}
	var found int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM execution_attachments WHERE id=? AND user_id=? AND session_id=? FOR UPDATE`, id, userID, sessionID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var pins int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attachment_pins WHERE attachment_id=?`, id).Scan(&pins); err != nil {
		return err
	}
	if pins > 0 {
		return ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM execution_attachments WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *Repository) CleanupAttachments(ctx context.Context, limit int) (int, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT a.id,a.user_id,a.session_id FROM execution_attachments a WHERE expires_ms<=`+sqlNow+` AND NOT EXISTS(SELECT 1 FROM execution_attachment_pins p WHERE p.attachment_id=a.id) LIMIT ?`, max(1, limit))
	if err != nil {
		return 0, err
	}
	var items []Attachment
	for rows.Next() {
		var a Attachment
		if err = rows.Scan(&a.ID, &a.UserID, &a.SessionID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range items {
		err = r.DeleteAttachment(ctx, a.UserID, a.SessionID, a.ID)
		if err == nil {
			n++
		} else if !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

func (r *Repository) DeleteUser(ctx context.Context, userID int64) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=? FOR UPDATE`, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// Every async session mutation also holds the user row, so no new acceptance,
	// attachment pin, or worker transition can race this check and cascade.
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_evidence WHERE user_id=? AND state IN ('queued','running','uncertain')`, userID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
