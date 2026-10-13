package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	redisclient "github.com/redis/go-redis/v9"
	"unichatgo/internal/config"
	"unichatgo/internal/execution"
)

type backendProcess struct {
	cmd     *exec.Cmd
	done    chan error
	url     string
	logPath string
	stopped bool
}

func (p *backendProcess) stop(t *testing.T, graceful bool) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	if graceful {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	} else {
		_ = p.cmd.Process.Kill()
	}
	select {
	case err := <-p.done:
		if graceful && err != nil {
			data, _ := os.ReadFile(p.logPath)
			t.Fatalf("graceful shutdown: %v\n%s", err, data)
		}
	case <-time.After(12 * time.Second):
		p.cmd.Process.Kill()
		t.Fatal("backend process did not stop")
	}
}

// Opt-in subprocess test: local listeners and isolated admin-created MySQL
// schemas are required. No external endpoint or paid model is ever used.
func TestAsyncProcessIsolationRecoveryAndAttachments(t *testing.T) {
	if os.Getenv("UNICHATGO_TEST_PROCESS") != "1" {
		t.Skip("OS process fault test not executed: set UNICHATGO_TEST_PROCESS=1")
	}
	addr := os.Getenv("UNICHATGO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("real Redis required")
	}
	f := newAsyncAPIFixture(t)
	ctx := context.Background()
	var schema string
	if err := f.db.QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dsn, err := mysql.ParseDSN(os.Getenv("UNICHATGO_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	dbhost, dbport, err := net.SplitHostPort(dsn.Addr)
	if err != nil {
		t.Fatal(err)
	}
	dbportInt, _ := strconv.Atoi(dbport)
	rhost, rport, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	rportInt, _ := strconv.Atoi(rport)
	rc := redisclient.NewClient(&redisclient.Options{Addr: addr, DB: 14})
	defer rc.Close()
	if err = rc.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	stream := "unichatgo:execution:v1"
	if err = rc.Del(ctx, stream).Err(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	entered := make(chan string, 8)
	firstRelease := make(chan struct{})
	crashRelease := make(chan struct{})
	var firstOnce, crashOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
	releaseCrash := func() { crashOnce.Do(func() { close(crashRelease) }) }
	t.Cleanup(releaseFirst)
	t.Cleanup(releaseCrash)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		select {
		case entered <- string(body):
		case <-r.Context().Done():
			return
		default:
			t.Error("unexpected duplicate mock calls exceeded observation capacity")
			http.Error(w, "too many test calls", 500)
			return
		}
		if bytes.Contains(body, []byte("cross-host-turn")) {
			select {
			case <-firstRelease:
			case <-r.Context().Done():
				return
			}
		}
		if bytes.Contains(body, []byte("worker-crash-turn")) {
			select {
			case <-crashRelease:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"process answer"}}]}`)
	}))
	t.Cleanup(func() { releaseFirst(); releaseCrash(); mock.Close() })
	buildDir := t.TempDir()
	binary := filepath.Join(buildDir, "unichatgo")
	buildCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	build.Dir = filepath.Clean("../..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build backend: %v\n%s", err, output)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	processes := []*backendProcess{}
	t.Cleanup(func() {
		for _, p := range processes {
			if !p.stopped {
				p.stop(t, false)
			}
		}
	})
	start := func(role string) *backendProcess {
		work := t.TempDir()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listenAddr := listener.Addr().String()
		listener.Close()
		cfg := config.Config{BasicConfig: config.BasicConfig{ServerAddress: listenAddr, FileBaseDir: filepath.Join(work, "uploads")}, Providers: map[string]config.ProviderConfig{"openai": {Model: "mock-model", BaseURL: mock.URL}}, Databases: map[string]config.DatabaseConfig{"mysql": {Host: dbhost, Port: dbportInt, Username: dsn.User, Password: dsn.Passwd, DBName: schema, Params: "parseTime=true&charset=utf8mb4"}}, Redis: config.RedisConfig{Host: rhost, Port: rportInt, DB: 14}, Async: config.AsyncConfig{Enabled: true, Role: role, Workers: 1, TaskTimeoutSeconds: 120, Concurrency: 4, UserConcurrency: 2, RPM: 1000, TPM: 1000000}}
		data, _ := json.Marshal(cfg)
		cfgPath := filepath.Join(work, "config.json")
		if err = os.WriteFile(cfgPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(work, "process.log")
		logFile, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "UNICHATGO_DB=mysql", "UNICHATGO_CONFIG="+cfgPath, "GIN_MODE=release", "UNICHATGO_APIKEY_KEY=0123456789abcdef0123456789abcdef")
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err = cmd.Start(); err != nil {
			logFile.Close()
			t.Fatal(err)
		}
		p := &backendProcess{cmd: cmd, done: make(chan error, 1), url: "http://" + listenAddr, logPath: logPath}
		processes = append(processes, p)
		go func() { err := cmd.Wait(); logFile.Close(); p.done <- err }()
		if role != "worker" {
			waitCondition(t, 8*time.Second, func() bool {
				res, err := client.Get(p.url + "/readyz")
				if err != nil {
					return false
				}
				res.Body.Close()
				return res.StatusCode == 200
			})
		}
		return p
	}
	request := func(p *backendProcess, method, path string, payload any, want int) []byte {
		data, _ := json.Marshal(payload)
		req, err := http.NewRequest(method, p.url+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.token)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d want %d body=%s", method, path, res.StatusCode, want, body)
		}
		return body
	}
	apiA, apiB := start("api"), start("api")
	workerA, workerB := start("worker"), start("worker")
	newSession := func(p *backendProcess) int64 {
		body := request(p, http.MethodPost, fmt.Sprintf("/api/users/%d/conversation/start", f.user), map[string]any{"session_id": 0}, 202)
		var s struct {
			ID int64 `json:"sessionId"`
		}
		decodeJSON(t, body, &s)
		return s.ID
	}
	sid := newSession(apiA)
	var upload bytes.Buffer
	mw := multipart.NewWriter(&upload)
	_ = mw.WriteField("session_id", strconv.FormatInt(sid, 10))
	part, err := mw.CreateFormFile("file", "shared-note.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "cross-host attachment evidence")
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, apiA.url+fmt.Sprintf("/api/users/%d/uploads", f.user), &upload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	uploadBody, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatalf("upload: %d %s", res.StatusCode, uploadBody)
	}
	var attachment struct {
		ID int64 `json:"file_id"`
	}
	decodeJSON(t, uploadBody, &attachment)
	payload := f.input(sid, "process-cross-host", "cross-host-turn")
	payload["file_ids"] = []int64{attachment.ID}
	payload["attachment_namespace"] = "shared-v1"
	body := request(apiB, http.MethodPost, f.messagePath(), payload, 202)
	var receipt execution.Receipt
	decodeJSON(t, body, &receipt)
	select {
	case body := <-entered:
		if !strings.Contains(body, "cross-host attachment evidence") {
			t.Fatal("worker could not read API A upload")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("separate worker did not enter mock")
	}
	apiB.stop(t, false)
	releaseFirst()
	waitCondition(t, 10*time.Second, func() bool {
		e, err := f.repo.Get(ctx, receipt.ID)
		return err == nil && e.State == execution.Completed
	})
	apiB = start("api")
	body = request(apiB, http.MethodGet, f.historyPath(sid), nil, 200)
	if !bytes.Contains(body, []byte("process answer")) {
		t.Fatal("replacement API did not restore persisted answer")
	}
	request(apiB, http.MethodPost, f.messagePath(), payload, 200)
	if calls.Load() != 1 {
		t.Fatal("API restart duplicated upstream")
	}
	workerA.stop(t, true)
	workerB.stop(t, true)
	// Accepted intent survives Redis stream deletion while no worker is alive.
	sid2 := newSession(apiA)
	body = request(apiA, http.MethodPost, f.messagePath(), f.input(sid2, "redis-rebuild", "redis-rebuild-turn"), 202)
	decodeJSON(t, body, &receipt)
	if err = rc.Del(ctx, stream).Err(); err != nil {
		t.Fatal(err)
	}
	workerA = start("worker")
	workerB = start("worker")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("SQL outbox did not rebuild missing stream")
	}
	waitCondition(t, 10*time.Second, func() bool {
		e, err := f.repo.Get(ctx, receipt.ID)
		return err == nil && e.State == execution.Completed
	})
	// Both child workers are forcibly killed after the mock proves call entry.
	sid3 := newSession(apiA)
	body = request(apiA, http.MethodPost, f.messagePath(), f.input(sid3, "worker-crash", "worker-crash-turn"), 202)
	decodeJSON(t, body, &receipt)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("crash call never entered")
	}
	workerA.stop(t, false)
	workerB.stop(t, false)
	releaseCrash()
	if _, err = f.db.Exec(`UPDATE execution_evidence SET lease_until_ms=0 WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	workerA = start("worker")
	waitCondition(t, 10*time.Second, func() bool {
		e, err := f.repo.Get(ctx, receipt.ID)
		return err == nil && e.State == execution.Uncertain
	})
	request(apiB, http.MethodPost, f.messagePath(), f.input(sid3, "blocked-after-crash", "must not run"), 409)
	if calls.Load() != 3 {
		t.Fatalf("crash or redis rebuild duplicated provider: calls=%d", calls.Load())
	}
	body = request(apiB, http.MethodGet, f.historyPath(sid3), nil, 200)
	if !bytes.Contains(body, []byte(`"state":"uncertain"`)) {
		t.Fatalf("uncertain not visible %s", body)
	}
	workerA.stop(t, true)
	apiA.stop(t, true)
	apiB.stop(t, true)
	t.Log("two API and two worker OS processes in separate local directories: shared SQL upload, API SIGKILL/restart, worker SIGKILL->uncertain, Redis stream rebuild, graceful drain verified; 3 mock calls")
}

func waitCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("bounded state-observation deadline exceeded")
		case <-tick.C:
		}
	}
}
