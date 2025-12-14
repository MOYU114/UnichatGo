package assistant

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/models"
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

func TestListAndDeleteUserTokens(t *testing.T) {
	t.Setenv(apiTokenKeyEnv, strings.Repeat("c", 32))
	db := openTestDB(t)
	defer db.Close()

	svc, err := NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	userID := insertTestUser(t, db, "carol")
	ctx := context.Background()

	if err := svc.SetUserToken(ctx, userID, "openai", "token-1"); err != nil {
		t.Fatalf("set token openai: %v", err)
	}
	if err := svc.SetUserToken(ctx, userID, "gemini", "token-2"); err != nil {
		t.Fatalf("set token gemini: %v", err)
	}

	tokens, err := svc.ListUserTokens(ctx, userID)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}

	if err := svc.DeleteUserToken(ctx, userID, "openai"); err != nil {
		t.Fatalf("delete token: %v", err)
	}
	// ensure token removed
	tokens, err = svc.ListUserTokens(ctx, userID)
	if err != nil {
		t.Fatalf("list tokens after delete: %v", err)
	}
	if len(tokens) != 1 || tokens[0].Provider != "gemini" {
		t.Fatalf("unexpected tokens after delete: %+v", tokens)
	}

	if err := svc.DeleteUserToken(ctx, userID, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestTempFileLookupAndSummaryUpdate(t *testing.T) {
	t.Setenv(apiTokenKeyEnv, strings.Repeat("d", 32))
	db := openTestDB(t)
	defer db.Close()

	svc, err := NewService(db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	userID := insertTestUser(t, db, "dave")
	session, err := svc.CreateSession(context.Background(), userID, "temp")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	fileID, err := svc.RecordTempFile(context.Background(), userID, session.ID, "doc.txt", "/tmp/doc.txt", "text/plain", 10, time.Hour)
	if err != nil {
		t.Fatalf("record temp file: %v", err)
	}

	files, err := svc.GetTempFilesByIDs(context.Background(), userID, session.ID, []int64{fileID})
	if err != nil {
		t.Fatalf("GetTempFilesByIDs: %v", err)
	}
	if len(files) != 1 || files[0].FileName != "doc.txt" {
		t.Fatalf("unexpected files: %+v", files)
	}

	msg, err := svc.AddMessage(context.Background(), models.Message{
		UserID:    userID,
		SessionID: session.ID,
		Role:      models.RoleSystem,
		Content:   "stub",
	})
	if err != nil {
		t.Fatalf("add message: %v", err)
	}

	if err := svc.UpdateTempFileSummary(context.Background(), fileID, "summary text", msg.ID); err != nil {
		t.Fatalf("UpdateTempFileSummary: %v", err)
	}
	var summary string
	var msgID int64
	if err := db.QueryRow(`SELECT summary, summary_message_id FROM temp_files WHERE id = ?`, fileID).Scan(&summary, &msgID); err != nil {
		t.Fatalf("scan temp file: %v", err)
	}
	if summary != "summary text" || msgID != msg.ID {
		t.Fatalf("unexpected summary or msg id: %q, %d", summary, msgID)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	cfg := &config.Config{
		Databases: map[string]config.DatabaseConfig{
			"sqlite3": {DSN: ":memory:"},
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

func insertTestUser(t *testing.T, db *sql.DB, username string) int64 {
	t.Helper()
	now := time.Now().UTC()
	res, err := db.Exec(`INSERT INTO users (username, password_hash, created_at) VALUES (?, '', ?)`, username, now)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}
