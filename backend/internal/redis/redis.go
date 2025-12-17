package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"unichatgo/internal/config"

	redis "github.com/redis/go-redis/v9"
)

// Client wraps go-redis client to centralize configuration.
type Client struct {
	inner *redis.Client
}

// ErrCacheMiss mirrors redis.Nil for callers.
var ErrCacheMiss = redis.Nil

// NewRedisClient creates the redis client from app config.
func NewRedisClient(cfg *config.Config) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("config required")
	}
	host := cfg.Redis.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Redis.Port
	if port == 0 {
		port = 6379
	}

	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", host, port),
		Username: cfg.Redis.Username,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	}

	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, err
	}
	return &Client{inner: client}, nil
}

// Set stores a key with TTL.
func (c *Client) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	if c == nil || c.inner == nil {
		return errors.New("redis client not initialized")
	}
	return c.inner.Set(ctx, key, value, ttl).Err()
}

// Get fetches the key as string.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	if c == nil || c.inner == nil {
		return "", errors.New("redis client not initialized")
	}
	return c.inner.Get(ctx, key).Result()
}

// Del removes provided keys.
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if c == nil || c.inner == nil {
		return errors.New("redis client not initialized")
	}
	if len(keys) == 0 {
		return nil
	}
	return c.inner.Del(ctx, keys...).Err()
}

// TTL returns key ttl.
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	if c == nil || c.inner == nil {
		return 0, errors.New("redis client not initialized")
	}
	return c.inner.TTL(ctx, key).Result()
}

// Close closes client.
func (c *Client) Close() error {
	if c == nil || c.inner == nil {
		return nil
	}
	return c.inner.Close()
}

// Raw exposes underlying go-redis client.
func (c *Client) Raw() *redis.Client {
	if c == nil {
		return nil
	}
	return c.inner
}
