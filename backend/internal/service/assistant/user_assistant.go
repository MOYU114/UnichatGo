package assistant

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"unichatgo/internal/models"
)

// Service handles user lifecycle and input persistence.
type Service struct {
	db *sql.DB
}

// NewService builds a new assistant service.
func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// RegisterUser creates a user with the supplied credentials.
func (s *Service) RegisterUser(ctx context.Context, username, password string) (*models.User, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}

	hash := hashPassword(password)
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, hash, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}
	return &models.User{ID: id, Username: username, PasswordHash: hash, CreatedAt: now}, nil
}

// Login validates credentials and returns the user profile.
func (s *Service) Login(ctx context.Context, username, password string) (*models.User, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`, username,
	)
	var user models.User
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("invalid credentials")
		}
		return nil, fmt.Errorf("query user: %w", err)
	}

	if user.PasswordHash != hashPassword(password) {
		return nil, errors.New("invalid credentials")
	}
	return &user, nil
}

// DeleteUser removes a user and cascaded data.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("invalid user id")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AppendMessageToSession persists a message for an existing session/user pair.
func (s *Service) AppendMessageToSession(ctx context.Context, userID, sessionID int64, role models.Role, content string) (*models.Message, error) {
	if userID <= 0 {
		return nil, errors.New("user_id is required")
	}
	if sessionID <= 0 {
		return nil, errors.New("session_id is required")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("content cannot be empty")
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ? AND user_id = ?)`,
		sessionID, userID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("verify session: %w", err)
	}
	if !exists {
		return nil, errors.New("session not found")
	}

	msg := models.Message{
		UserID:    userID,
		SessionID: sessionID,
		Role:      role,
		Content:   content,
	}
	return s.AddMessage(ctx, msg)
}

func hashPassword(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// EnsureAIReady verifies that the user has configured a token for the provider.
func (s *Service) EnsureAIReady(ctx context.Context, userID int64, provider string) (string, error) {
	token, err := s.HasUserToken(ctx, userID, provider)
	if err != nil {
		return "", err
	}
	// Must have api token.
	if token == "" {
		return "", errors.New("api token not configured")
	}
	return token, nil
}

// HasUserToken returns the API token stored for the user/provider pair. Don't require have api token.
func (s *Service) HasUserToken(ctx context.Context, userID int64, provider string) (string, error) {
	if userID <= 0 {
		return "", errors.New("invalid user id")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "", errors.New("provider is required")
	}
	var token string
	err := s.db.QueryRowContext(
		ctx,
		`SELECT api_key FROM apiKeys WHERE user_id = ? AND provider = ? LIMIT 1`,
		userID,
		provider,
	).Scan(&token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("lookup api token: %w", err)
	}
	return token, nil
}

// SetUserToken persists or updates the API token for a user/provider pair.
func (s *Service) SetUserToken(ctx context.Context, userID int64, provider, token string) error {
	if userID <= 0 {
		return errors.New("invalid user id")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return errors.New("provider is required")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("token is required")
	}

	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = ?)`, userID).Scan(&exists); err != nil {
		return fmt.Errorf("verify user: %w", err)
	}
	if !exists {
		return errors.New("user not found")
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO apiKeys (user_id, provider, api_key, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, provider) DO UPDATE SET api_key = excluded.api_key, created_at = excluded.created_at`,
		userID, provider, token, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	return nil
}

// DeleteUserToken removes the stored token for a user/provider pair.
func (s *Service) DeleteUserToken(ctx context.Context, userID int64, provider string) error {
	if userID <= 0 {
		return errors.New("invalid user id")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return errors.New("provider is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM apiKeys WHERE user_id = ? AND provider = ?`, userID, provider)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return errors.New("token not found")
	}
	return nil
}
