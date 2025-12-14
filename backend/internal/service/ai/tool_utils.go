package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"unichatgo/internal/models"
)

const (
	TempFileChunkSizeDefault = 1000
	TempFileChunkSizeMin     = 500
	TempFileChunkSizeMax     = 2000
	TempFileRateLimit        = 3
	TempFileRateWindow       = time.Minute
	WebSearchHTTPTimeout     = 10 * time.Second
)

type tempFileContextKey struct{}
type toolSessionContextKey struct{}

type toolRateLimiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	hits   map[string][]time.Time
}

func newToolRateLimiter(limit int, window time.Duration) *toolRateLimiter {
	return &toolRateLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

func (l *toolRateLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	queue := l.hits[key]
	cutoff := now.Add(-l.window)
	idx := 0
	for _, t := range queue {
		if t.After(cutoff) {
			break
		}
		idx++
	}
	if idx > 0 {
		queue = queue[idx:]
	}
	if len(queue) >= l.limit {
		l.hits[key] = queue
		return false
	}
	queue = append(queue, now)
	l.hits[key] = queue
	return true
}

func WithTempFiles(ctx context.Context, files []*models.TempFile) context.Context {
	if len(files) == 0 {
		return ctx
	}
	copied := make([]*models.TempFile, 0, len(files))
	for _, f := range files {
		if f == nil {
			continue
		}
		c := *f
		copied = append(copied, &c)
	}
	return context.WithValue(ctx, tempFileContextKey{}, copied)
}

func TempFilesFromContext(ctx context.Context) []*models.TempFile {
	val := ctx.Value(tempFileContextKey{})
	if val == nil {
		return nil
	}
	files, _ := val.([]*models.TempFile)
	return files
}

func WithToolSession(ctx context.Context, userID, sessionID int64) context.Context {
	if userID <= 0 || sessionID <= 0 {
		return ctx
	}
	meta := struct {
		UserID    int64
		SessionID int64
	}{userID, sessionID}
	return context.WithValue(ctx, toolSessionContextKey{}, meta)
}

func ToolSessionFromContext(ctx context.Context) (int64, int64, bool) {
	val := ctx.Value(toolSessionContextKey{})
	if val == nil {
		return 0, 0, false
	}
	meta, ok := val.(struct {
		UserID    int64
		SessionID int64
	})
	if !ok {
		return 0, 0, false
	}
	return meta.UserID, meta.SessionID, true
}

func (w *webSearchTool) fetchURL(ctx context.Context, target string) (string, error) {
	if w.httpClient == nil {
		w.httpClient = &http.Client{Timeout: WebSearchHTTPTimeout}
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("unsupported url scheme")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "UnichatGo-WebSearch/1.0")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch url: %s", resp.Status)
	}

	const maxBodySize = 512 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func looksLikeURL(input string) bool {
	lower := strings.ToLower(input)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
