package auth

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/redis"
	"unichatgo/internal/storage"
)

func TestAuthIssueValidateRevoke(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	insertUser(t, db, 1)

	svc := NewService(db, nil, time.Hour)
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

	svc := NewService(db, nil, 10*time.Millisecond)
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

func TestAuthTokenCacheUsesRedis(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	insertUser(t, db, 10)

	cacheClient, cleanup := newRedisCacheClient(t)
	defer cleanup()

	svc := NewService(db, cacheClient, time.Hour)
	ctx := context.Background()

	token, err := svc.IssueToken(ctx, 10)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	raw := cacheClient.Raw()
	if raw == nil {
		t.Fatalf("redis raw client nil")
	}
	key := redisTokenPrefix + token
	got, err := raw.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("get redis token: %v", err)
	}
	if got != "10" {
		t.Fatalf("expected user 10 in rdb, got %s", got)
	}

	_, _ = db.Exec(`DELETE FROM user_tokens WHERE token = ?`, token)
	userID, err := svc.ValidateToken(ctx, token)
	if err != nil || userID != 10 {
		t.Fatalf("ValidateToken via rdb failed: id=%d err=%v", userID, err)
	}

	if err := svc.RevokeToken(ctx, token); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := raw.Get(ctx, key).Result(); err == nil {
		t.Fatalf("expected redis key deleted")
	}
	if _, err := svc.ValidateToken(ctx, token); err == nil {
		t.Fatalf("expected error after revoke and rdb delete")
	}
}

func newRedisCacheClient(t *testing.T) (*redis.Client, func()) {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run redis-backed auth tests")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port: %v", err)
	}
	db := 0
	if v := os.Getenv("TEST_REDIS_DB"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			db = parsed
		}
	}
	cfg := &config.Config{
		Redis: config.RedisConfig{
			Host: host,
			Port: port,
			DB:   db,
		},
	}
	client, err := redis.NewRedisClient(cfg)
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if raw := client.Raw(); raw != nil {
		if err := raw.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("flush db: %v", err)
		}
	}
	cleanup := func() {
		client.Close()
	}
	return client, cleanup
}
