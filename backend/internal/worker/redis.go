package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"unichatgo/internal/models"
	"unichatgo/internal/redis"
)

const (
	redisInvalidateChannel = "worker:invalidate"
	redisStateTTL          = 30 * time.Minute
)

const (
	scopeUser    = "user"
	scopeSession = "session"
	scopeFiles   = "files"
)

type invalidateMessage struct {
	UserID    int64  `json:"user_id"`
	SessionID int64  `json:"session_id"`
	Scope     string `json:"scope"`
}

type stateRedis struct {
	client *redis.Client
}

func newStateCache(client *redis.Client) *stateRedis {
	return &stateRedis{client: client}
}

// startListener redis listener using sub chan
func (r *stateRedis) startListener(handler func(invalidateMessage)) {
	if r == nil || r.client == nil || handler == nil {
		return
	}
	raw := r.client.Raw()
	if raw == nil {
		return
	}
	go func() {
		ctx := context.Background()
		pubsub := raw.Subscribe(ctx, redisInvalidateChannel)
		ch := pubsub.Channel()
		// use sub chan to receive msg
		for msg := range ch {
			var inv invalidateMessage
			if err := json.Unmarshal([]byte(msg.Payload), &inv); err != nil {
				log.Printf("worker invalidation decode failed: %v", err)
				continue
			}
			handler(inv)
		}
	}()
}

// publishInvalidation broadcast invalidate msg
func (r *stateRedis) publishInvalidation(msg invalidateMessage) {
	if r == nil || r.client == nil {
		return
	}
	raw := r.client.Raw()
	if raw == nil {
		return
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("worker invalidation marshal failed: %v", err)
		return
	}
	if err := raw.Publish(context.Background(), redisInvalidateChannel, payload).Err(); err != nil {
		log.Printf("worker publish invalidation failed: %v", err)
	}
}

func (r *stateRedis) cacheSession(session *models.Session, history []*models.Message) {
	if r == nil || r.client == nil || session == nil || session.ID <= 0 {
		return
	}
	ctx := context.Background()
	data, err := json.Marshal(session)
	if err == nil {
		key := fmt.Sprintf("worker:session:%d", session.ID)
		if err := r.client.Set(ctx, key, data, redisStateTTL); err != nil {
			log.Printf("worker rdb session failed: %v", err)
		}
	}
	r.cacheHistory(session.ID, history)
}

func (r *stateRedis) cacheHistory(sessionID int64, history []*models.Message) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return
	}
	ctx := context.Background()
	data, err := json.Marshal(history)
	if err != nil {
		log.Printf("worker rdb history marshal failed: %v", err)
		return
	}
	key := fmt.Sprintf("worker:history:%d", sessionID)
	if err := r.client.Set(ctx, key, data, redisStateTTL); err != nil {
		log.Printf("worker rdb history failed: %v", err)
	}
}

func (r *stateRedis) cacheFiles(sessionID int64, files []*models.TempFile) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return
	}
	if len(files) == 0 {
		r.invalidateFiles(sessionID)
		return
	}
	ctx := context.Background()
	data, err := json.Marshal(files)
	if err != nil {
		log.Printf("worker rdb files marshal failed: %v", err)
		return
	}
	key := fmt.Sprintf("worker:files:%d", sessionID)
	if err := r.client.Set(ctx, key, data, redisStateTTL); err != nil {
		log.Printf("worker rdb files failed: %v", err)
	}
}

func (r *stateRedis) loadSession(userID, sessionID int64) (*models.Session, []*models.Message, bool) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return nil, nil, false
	}
	ctx := context.Background()
	key := fmt.Sprintf("worker:session:%d", sessionID)
	rawSession, err := r.client.Get(ctx, key)
	if err != nil {
		if err != redis.ErrCacheMiss {
			log.Printf("worker load session rdb failed: %v", err)
		}
		return nil, nil, false
	}
	var session models.Session
	if err := json.Unmarshal([]byte(rawSession), &session); err != nil {
		log.Printf("worker decode session rdb failed: %v", err)
		return nil, nil, false
	}
	if session.UserID != userID {
		return nil, nil, false
	}

	var history []*models.Message
	historyKey := fmt.Sprintf("worker:history:%d", sessionID)
	rawHistory, err := r.client.Get(ctx, historyKey)
	if err == nil {
		if err := json.Unmarshal([]byte(rawHistory), &history); err != nil {
			log.Printf("worker decode history rdb failed: %v", err)
			history = nil
		}
	} else if err != redis.ErrCacheMiss {
		log.Printf("worker load history rdb failed: %v", err)
	}
	return &session, history, true
}

func (r *stateRedis) loadFiles(userID, sessionID int64) ([]*models.TempFile, bool) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return nil, false
	}
	ctx := context.Background()
	key := fmt.Sprintf("worker:files:%d", sessionID)
	rawFiles, err := r.client.Get(ctx, key)
	if err != nil {
		if err != redis.ErrCacheMiss {
			log.Printf("worker load files rdb failed: %v", err)
		}
		return nil, false
	}
	var files []*models.TempFile
	if err := json.Unmarshal([]byte(rawFiles), &files); err != nil {
		log.Printf("worker decode files rdb failed: %v", err)
		return nil, false
	}
	for _, f := range files {
		if f != nil && f.UserID != userID {
			return nil, false
		}
	}
	return files, true
}

func (r *stateRedis) invalidateSession(sessionID int64) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return
	}
	ctx := context.Background()
	sessionKey := fmt.Sprintf("worker:session:%d", sessionID)
	historyKey := fmt.Sprintf("worker:history:%d", sessionID)
	if err := r.client.Del(ctx, sessionKey, historyKey); err != nil && err != redis.ErrCacheMiss {
		log.Printf("worker invalidate session rdb failed: %v", err)
	}
}

func (r *stateRedis) invalidateFiles(sessionID int64) {
	if r == nil || r.client == nil || sessionID <= 0 {
		return
	}
	ctx := context.Background()
	key := fmt.Sprintf("worker:files:%d", sessionID)
	if err := r.client.Del(ctx, key); err != nil && err != redis.ErrCacheMiss {
		log.Printf("worker invalidate files rdb failed: %v", err)
	}
}
