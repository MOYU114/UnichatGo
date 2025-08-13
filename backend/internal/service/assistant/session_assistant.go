package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete session: %w", err)
	}
	return nil
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
