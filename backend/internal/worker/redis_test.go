package worker

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/models"
	"unichatgo/internal/redis"
)

func TestStateCacheStoreLoadAndInvalidate(t *testing.T) {
	sc, cleanup := newRedisStateCache(t)
	defer cleanup()

	session := &models.Session{ID: 101, UserID: 77, Title: "demo"}
	history := []*models.Message{
		{ID: 1, UserID: 77, SessionID: 101, Role: models.RoleUser, Content: "hello"},
	}
	files := []*models.TempFile{
		{ID: 9, UserID: 77, SessionID: 101, FileName: "doc.txt", StoredPath: "/tmp/doc.txt", MimeType: "text/plain"},
	}

	sc.cacheSession(session, history)
	sc.cacheFiles(session.ID, files)

	gotSession, gotHistory, ok := sc.loadSession(77, 101)
	if !ok || gotSession == nil {
		t.Fatalf("expected session cached")
	}
	if gotSession.Title != session.Title {
		t.Fatalf("session title mismatch: want %s got %s", session.Title, gotSession.Title)
	}
	if len(gotHistory) != len(history) {
		t.Fatalf("history mismatch: want %d got %d", len(history), len(gotHistory))
	}

	gotFiles, ok := sc.loadFiles(77, 101)
	if !ok || len(gotFiles) != len(files) {
		t.Fatalf("files not cached")
	}

	sc.invalidateSession(101)
	if _, _, ok := sc.loadSession(77, 101); ok {
		t.Fatalf("expected session rdb invalidated")
	}
	sc.invalidateFiles(101)
	if _, ok := sc.loadFiles(77, 101); ok {
		t.Fatalf("expected files rdb invalidated")
	}
}

func TestStateCachePubSub(t *testing.T) {
	sc, cleanup := newRedisStateCache(t)
	defer cleanup()

	ch := make(chan invalidateMessage, 1)
	sc.startListener(func(msg invalidateMessage) {
		ch <- msg
	})

	msg := invalidateMessage{UserID: 5, SessionID: 6, Scope: scopeSession}
	sc.publishInvalidation(msg)
	select {
	case got := <-ch:
		if got != msg {
			t.Fatalf("unexpected message %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive pubsub message")
	}
}

func newRedisStateCache(t *testing.T) (*stateRedis, func()) {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run redis-backed worker tests")
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
	if raw := client.Raw(); raw != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := raw.FlushDB(ctx).Err(); err != nil {
			t.Fatalf("flush db: %v", err)
		}
	}
	sc := newStateCache(client)
	cleanup := func() {
		client.Close()
	}
	return sc, cleanup
}
