package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"unichatgo/internal/redis"
)

// Service issues, validates, and revokes user authentication tokens.
type Service struct {
	db             *sql.DB
	rdb            *redis.Client
	tokenTTL       time.Duration
	cookieName     string
	headerName     string
	csrfCookieName string
	csrfHeaderName string
}

const redisTokenPrefix = "auth:token:"

// NewService constructs an auth service with the supplied token lifetime.
func NewService(db *sql.DB, cacheClient *redis.Client, ttl time.Duration) *Service {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Service{
		db:             db,
		rdb:            cacheClient,
		tokenTTL:       ttl,
		cookieName:     "auth_token",
		headerName:     "Authorization",
		csrfCookieName: "csrf_token",
		csrfHeaderName: "X-CSRF-Token",
	}
}

// IssueToken mints a new random token for the user and persists it.
func (s *Service) IssueToken(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", errors.New("invalid user id")
	}
	now := time.Now().UTC()
	expiresAt := now.Add(s.tokenTTL)
	// retry maximum 5 times
	for i := 0; i < 5; i++ {
		token, err := generateToken()
		if err != nil {
			return "", err
		}
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO user_tokens (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
			token, userID, now, expiresAt,
		)
		if err == nil {
			s.cacheToken(ctx, token, userID, expiresAt.Sub(now))
			return token, nil
		}
	}
	return "", errors.New("could not issue token")
}

// NewCSRFToken returns a random token used for CSRF protection.
func (s *Service) NewCSRFToken() (string, error) {
	return generateToken()
}

// ValidateToken verifies the token exists and has not expired, returning the user id.
func (s *Service) ValidateToken(ctx context.Context, authToken string) (int64, error) {
	if authToken == "" {
		return 0, errors.New("token required")
	}
	if userID, ok := s.lookupCacheToken(ctx, authToken); ok {
		return userID, nil
	}
	var userID int64
	var expires time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM user_tokens WHERE token = ?`, authToken,
	).Scan(&userID, &expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("invalid token")
		}
		return 0, fmt.Errorf("lookup token: %w", err)
	}
	if time.Now().UTC().After(expires) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE token = ?`, authToken)
		s.deleteCacheToken(ctx, authToken)
		return 0, errors.New("token expired")
	}
	s.cacheToken(ctx, authToken, userID, time.Until(expires))
	return userID, nil
}

// RevokeToken deletes a single token.
func (s *Service) RevokeToken(ctx context.Context, authToken string) error {
	if authToken == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE token = ?`, authToken); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	s.deleteCacheToken(ctx, authToken)
	return nil
}

// RevokeUserTokens removes all tokens belonging to the user.
func (s *Service) RevokeUserTokens(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return nil
	}
	tokens := s.collectUserTokens(ctx, userID)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("revoke user tokens: %w", err)
	}
	for _, token := range tokens {
		s.deleteCacheToken(ctx, token)
	}
	return nil
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// AuthCookieName returns the cookie name storing auth tokens.
func (s *Service) AuthCookieName() string {
	return s.cookieName
}

// CSRFCookieName returns the cookie used for CSRF tokens.
func (s *Service) CSRFCookieName() string {
	return s.csrfCookieName
}

// CSRFHeaderName returns the CSRF header name.
func (s *Service) CSRFHeaderName() string {
	return s.csrfHeaderName
}

// TokenTTL reports the configured token lifetime.
func (s *Service) TokenTTL() time.Duration {
	return s.tokenTTL
}

// For Redis rdb

func (s *Service) cacheToken(ctx context.Context, token string, userID int64, ttl time.Duration) {
	if s.rdb == nil || token == "" || userID <= 0 {
		return
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	if err := s.rdb.Set(ctx, redisTokenPrefix+token, strconv.FormatInt(userID, 10), ttl); err != nil {
		log.Printf("redis set token failed: %v", err)
	}
}

func (s *Service) lookupCacheToken(ctx context.Context, token string) (int64, bool) {
	if s.rdb == nil || token == "" {
		return 0, false
	}
	val, err := s.rdb.Get(ctx, redisTokenPrefix+token)
	if err != nil {
		if err != redis.ErrCacheMiss {
			log.Printf("redis get token failed: %v", err)
		}
		return 0, false
	}
	id, parseErr := strconv.ParseInt(val, 10, 64)
	if parseErr != nil {
		return 0, false
	}
	return id, true
}

func (s *Service) deleteCacheToken(ctx context.Context, token string) {
	if s.rdb == nil || token == "" {
		return
	}
	if err := s.rdb.Del(ctx, redisTokenPrefix+token); err != nil && err != redis.ErrCacheMiss {
		log.Printf("redis delete token failed: %v", err)
	}
}

func (s *Service) collectUserTokens(ctx context.Context, userID int64) []string {
	if s.rdb == nil || userID <= 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token FROM user_tokens WHERE user_id = ?`, userID)
	if err != nil {
		log.Printf("list user tokens for rdb delete failed: %v", err)
		return nil
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			log.Printf("scan token for rdb delete failed: %v", err)
			return tokens
		}
		tokens = append(tokens, token)
	}
	return tokens
}
