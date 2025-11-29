package assistant

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"unichatgo/internal/storage"
)

func TestSetUserTokenEncryptsData(t *testing.T) {
	t.Setenv(apiTokenKeyEnv, strings.Repeat("a", 32))
	db := openTestDB(t)
	defer db.Close()

	svc, err := NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	userID := insertTestUser(t, db, "alice")
	if err := svc.SetUserToken(context.Background(), userID, "openai", "secret-token"); err != nil {
		t.Fatalf("set token: %v", err)
	}
	var stored string
	if err := db.QueryRow(`SELECT api_key FROM apiKeys WHERE user_id = ? AND provider = ?`, userID, "openai").Scan(&stored); err != nil {
		t.Fatalf("query stored token: %v", err)
	}
	if stored == "secret-token" {
		t.Fatalf("token stored in plaintext")
	}
	got, err := svc.HasUserToken(context.Background(), userID, "openai")
	if err != nil {
		t.Fatalf("has user token: %v", err)
	}
	if got != "secret-token" {
		t.Fatalf("expected decrypted token, got %q", got)
	}
}

func TestHasUserTokenAllowsLegacyPlaintext(t *testing.T) {
	t.Setenv(apiTokenKeyEnv, strings.Repeat("b", 32))
	db := openTestDB(t)
	defer db.Close()

	svc, err := NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	userID := insertTestUser(t, db, "bob")
	legacy := "legacy-token"
	if _, err := db.Exec(`INSERT INTO apiKeys (user_id, provider, api_key, created_at) VALUES (?, ?, ?, ?)`, userID, "openai", legacy, time.Now()); err != nil {
		t.Fatalf("insert legacy token: %v", err)
	}
	got, err := svc.HasUserToken(context.Background(), userID, "openai")
	if err != nil {
		t.Fatalf("HasUserToken: %v", err)
	}
	if got != legacy {
		t.Fatalf("expected legacy token, got %q", got)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func insertTestUser(t *testing.T, db *sql.DB, username string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at) VALUES (?, '', datetime('now'))`, username)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}
