package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/storage"
)

func TestAuthIssueValidateRevoke(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	insertUser(t, db, 1)

	svc := NewService(db, time.Hour)
	token, err := svc.IssueToken(context.Background(), 1)
	if err != nil {
		t.Fatalf("IssueToken error: %v", err)
	}
	if token == "" {
		t.Fatalf("expected token")
	}
	userID, err := svc.ValidateToken(context.Background(), token)
	if err != nil || userID != 1 {
		t.Fatalf("ValidateToken failed: id=%d err=%v", userID, err)
	}
	if err := svc.RevokeToken(context.Background(), token); err != nil {
		t.Fatalf("RevokeToken error: %v", err)
	}
	if _, err := svc.ValidateToken(context.Background(), token); err == nil {
		t.Fatalf("expected error after revoke")
	}

	token2, err := svc.IssueToken(context.Background(), 1)
	if err != nil {
		t.Fatalf("IssueToken error: %v", err)
	}
	if err := svc.RevokeUserTokens(context.Background(), 1); err != nil {
		t.Fatalf("RevokeUserTokens error: %v", err)
	}
	if _, err := svc.ValidateToken(context.Background(), token2); err == nil {
		t.Fatalf("expected error after revoke all")
	}
}

func TestAuthValidateExpiredToken(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	insertUser(t, db, 2)

	svc := NewService(db, 10*time.Millisecond)
	token, err := svc.IssueToken(context.Background(), 2)
	if err != nil {
		t.Fatalf("IssueToken error: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := svc.ValidateToken(context.Background(), token); err == nil {
		t.Fatalf("expected expiration error")
	}
	// ensure token removed
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_tokens WHERE token = ?`, token).Scan(&count); err != nil {
		t.Fatalf("query tokens: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired token not purged")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := &config.Config{
		Databases: map[string]config.DatabaseConfig{
			"sqlite3": {
				DSN: ":memory:",
			},
		},
	}
	db, err := storage.Open("sqlite3", cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.Migrate(db, "sqlite3"); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func insertUser(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, '', ?)`,
		id, "user_"+time.Now().Format("150405"), time.Now().UTC())
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
}
