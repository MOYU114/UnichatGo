package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"unichatgo/internal/models"
)

// CreateSession inserts a new session for the given user/provider and returns the record.
func (s *Service) CreateSession(ctx context.Context, userID int64, title string) (*models.Session, error) {
	if userID <= 0 {
		return nil, errors.New("user_id is required")
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		userID, title, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("session id: %w", err)
	}
	return &models.Session{ID: id, UserID: userID, Title: title, CreatedAt: now, UpdatedAt: now}, nil
}

// ListSessions returns all sessions for a user ordered by last activity.
func (s *Service) ListSessions(ctx context.Context, userID int64) ([]models.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, title, created_at, updated_at FROM sessions WHERE user_id = ? ORDER BY updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []models.Session
	for rows.Next() {
		var s models.Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.Title, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// GetSessionWithMessages returns one session and its ordered messages.
func (s *Service) GetSessionWithMessages(ctx context.Context, userID, sessionID int64) (*models.Session, []*models.Message, error) {
	var session models.Session
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, title, created_at, updated_at FROM sessions WHERE id = ? AND user_id = ?`,
		sessionID,
		userID,
	).Scan(&session.ID, &session.UserID, &session.Title, &session.CreatedAt, &session.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("get session: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, session_id, role, content, created_at FROM messages WHERE session_id = ? ORDER BY created_at ASC`,
		sessionID,
	)
	if err != nil {
		return &session, nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var messages []*models.Message
	for rows.Next() {
		m := new(models.Message)
		if err := rows.Scan(&m.ID, &m.UserID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return &session, nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return &session, messages, rows.Err()
}

// AddMessage stores a new message and updates the session's updated_at timestamp.
func (s *Service) AddMessage(ctx context.Context, msg models.Message) (*models.Message, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (user_id, session_id, role, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		msg.UserID, msg.SessionID, msg.Role, msg.Content, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("message id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, msg.SessionID); err != nil {
		return nil, fmt.Errorf("touch session: %w", err)
	}
	msg.ID = id
	msg.CreatedAt = now
	return &msg, nil
}

// DeleteSession removes a session and all related messages for the user.
func (s *Service) DeleteSession(ctx context.Context, userID, sessionID int64) error {
	if sessionID <= 0 {
		return errors.New("invalid session id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	paths, err := s.collectTempFilePaths(ctx, tx, sessionID)
	if err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, sessionID, userID)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("session rows affected: %w", err)
	}
	if affected == 0 {
		tx.Rollback()
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM temp_files WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete temp files: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete session: %w", err)
	}

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("remove temp file %s failed: %v", path, err)
		}
	}
	return nil
}

func (s *Service) collectTempFilePaths(ctx context.Context, tx *sql.Tx, sessionID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT stored_path FROM temp_files WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list temp files for delete: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan temp file path: %w", err)
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate temp file paths: %w", err)
	}
	return paths, nil
}

// UpdateSessionTitle sets a session title for the specified user.
func (s *Service) UpdateSessionTitle(ctx context.Context, userID, sessionID int64, title string) error {
	if sessionID <= 0 {
		return errors.New("invalid session id")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title cannot be empty")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ? WHERE id = ? AND user_id = ?`,
		title, sessionID, userID,
	)
	if err != nil {
		return fmt.Errorf("update session title: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("session rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) RecordTempFile(ctx context.Context, userID, sessionID int64, displayName, storedPath, mime string, size int64, ttl time.Duration) (int64, error) {
	if userID <= 0 || sessionID <= 0 {
		return 0, errors.New("invalid identifiers")
	}
	if displayName == "" || storedPath == "" {
		return 0, errors.New("invalid file metadata")
	}
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = DefaultTempFileTTL
	}
	expire := now.Add(ttl)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO temp_files (user_id, session_id, file_name, stored_path, mime_type, size, status, summary, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', '', ?, ?)`,
		userID, sessionID, displayName, storedPath, mime, size, now, expire,
	)
	if err != nil {
		return 0, fmt.Errorf("record temp file: %w", err)
	}
	fileID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("temp file id: %w", err)
	}
	return fileID, nil
}

func (s *Service) TempStorageUsage(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, errors.New("invalid user id")
	}
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size),0) FROM temp_files WHERE user_id = ? AND status = 'active'`,
		userID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("temp storage usage: %w", err)
	}
	return total.Int64, nil
}

func (s *Service) GetTempFilesByIDs(ctx context.Context, userID, sessionID int64, ids []int64) ([]*models.TempFile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if userID <= 0 || sessionID <= 0 {
		return nil, errors.New("invalid identifiers")
	}
	now := time.Now().UTC()
	placeholders := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids)+3)
	args = append(args, userID, sessionID, now)
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("invalid file id")
		}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT %s FROM temp_files
		WHERE user_id = ? AND session_id = ? AND status = 'active' AND expires_at > ?
		AND id IN (%s)`,
		tempFileColumns, strings.Join(placeholders, ","))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list temp files: %w", err)
	}
	defer rows.Close()

	var files []*models.TempFile
	for rows.Next() {
		tf, err := scanTempFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, tf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate temp files: %w", err)
	}
	return files, nil
}

func (s *Service) ListSessionTempFiles(ctx context.Context, userID, sessionID int64) ([]*models.TempFile, error) {
	if userID <= 0 || sessionID <= 0 {
		return nil, errors.New("invalid identifiers")
	}
	now := time.Now().UTC()
	query := fmt.Sprintf(`SELECT %s FROM temp_files
		WHERE user_id = ? AND session_id = ? AND status = 'active' AND expires_at > ?
		ORDER BY created_at ASC`, tempFileColumns)
	rows, err := s.db.QueryContext(ctx, query, userID, sessionID, now)
	if err != nil {
		return nil, fmt.Errorf("list session temp files: %w", err)
	}
	defer rows.Close()

	var files []*models.TempFile
	for rows.Next() {
		tf, err := scanTempFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, tf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate temp files: %w", err)
	}
	return files, nil
}

func (s *Service) UpdateTempFileSummary(ctx context.Context, fileID int64, summary string, messageID int64) error {
	if fileID <= 0 {
		return errors.New("invalid file id")
	}
	summary = strings.TrimSpace(summary)
	var msgID sql.NullInt64
	if messageID > 0 {
		msgID = sql.NullInt64{Int64: messageID, Valid: true}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE temp_files SET summary = ?, summary_message_id = ? WHERE id = ?`,
		summary, msgID, fileID); err != nil {
		return fmt.Errorf("update temp file summary: %w", err)
	}
	return nil
}

const tempFileColumns = `
		id, user_id, session_id, file_name, stored_path, mime_type, size,
		status, summary, summary_message_id, created_at, expires_at
	`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTempFile(scanner rowScanner) (*models.TempFile, error) {
	var tf models.TempFile
	var summaryMsg sql.NullInt64
	if err := scanner.Scan(
		&tf.ID,
		&tf.UserID,
		&tf.SessionID,
		&tf.FileName,
		&tf.StoredPath,
		&tf.MimeType,
		&tf.Size,
		&tf.Status,
		&tf.Summary,
		&summaryMsg,
		&tf.CreatedAt,
		&tf.ExpiresAt,
	); err != nil {
		return nil, err
	}
	if summaryMsg.Valid {
		tf.SummaryMessageID = summaryMsg.Int64
	}
	return &tf, nil
}
