package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"unichatgo/internal/api"
	"unichatgo/internal/auth"
	"unichatgo/internal/config"
	"unichatgo/internal/execution"
	"unichatgo/internal/redis"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/storage"
	"unichatgo/internal/worker"

	"github.com/gin-gonic/gin"
)

func main() {
	cfgPath := os.Getenv("UNICHATGO_CONFIG")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	dbType := os.Getenv("UNICHATGO_DB")
	if dbType == "" {
		dbType = "sqlite3"
	}
	log.Printf("dbType: %s\n", dbType)
	db, err := storage.Open(dbType, cfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if cfg.Async.Enabled && dbType != "mysql" {
		log.Fatal("async execution requires MySQL")
	}
	if !cfg.Async.Enabled && dbType == "mysql" {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='execution_control'`).Scan(&exists); err != nil {
			log.Fatal(err)
		}
		if exists > 0 {
			log.Fatal("async migration installed: refusing legacy writer; use a compatible async version for rollback")
		}
	}
	rdb, err := redis.NewRedisClient(cfg)
	if err != nil {
		log.Fatalf("create redis client: %v", err)
	}
	defer rdb.Close()

	// Create necessary tables: users, apiKeys, sessions, messages
	if err := storage.Migrate(db, dbType); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	if cfg.Async.Enabled {
		if err := execution.Migrate(context.Background(), db); err != nil {
			log.Fatalf("async migrate: %v", err)
		}
	}

	assistantService, err := assistant.NewService(db)
	if err != nil {
		log.Fatalf("init assistant service: %v", err)
	}
	workerCfg := worker.DispatcherConfig{
		MinWorkers:        cfg.BasicConfig.MinWorkers,
		MaxWorkers:        cfg.BasicConfig.MaxWorkers,
		QueueSize:         cfg.BasicConfig.QueueSize,
		WorkerIdleTimeout: time.Duration(cfg.BasicConfig.WorkerIdleTimeout) * time.Minute,
	}
	cleanCtx, cleanCancel := context.WithCancel(context.Background())
	defer cleanCancel()
	cleanInterval := time.Duration(cfg.BasicConfig.TempCleanInterval) * time.Minute
	if cleanInterval <= 0 {
		cleanInterval = assistant.DefaultTempFileCleanupInterval
	}
	if !cfg.Async.Enabled {
		assistantService.StartTempFileCleaner(cleanCtx, cleanInterval)
	}
	authService := auth.NewService(db, rdb, 24*time.Hour)
	fileBase := cfg.BasicConfig.FileBaseDir
	if fileBase == "" {
		fileBase = "./data/uploads"
	}
	tempTTL := time.Duration(cfg.BasicConfig.TempFileTTL) * time.Minute
	if tempTTL <= 0 {
		tempTTL = assistant.DefaultTempFileTTL
	}
	var handlers *api.Handler
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var runtime *execution.Runtime
	workerOnly := false
	done := make(chan struct{})
	if cfg.Async.Enabled {
		role := cfg.Async.Role
		if role == "" {
			role = "all"
		}
		if role != "api" && role != "worker" && role != "all" {
			log.Fatal("invalid async role")
		}
		timeout := time.Duration(cfg.Async.TaskTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		repo := execution.NewRepository(db, execution.Limits{SessionQueue: cfg.Async.SessionQueue, UserQueue: cfg.Async.UserQueue, AcceptRPM: cfg.Async.AcceptRPM, TaskTimeout: timeout})
		if err := repo.CheckSchema(rootCtx); err != nil {
			log.Fatalf("async schema/write latch: %v", err)
		}
		if err := repo.BindAdmission(rootCtx); err != nil {
			log.Fatalf("admission configuration: %v", err)
		}
		handlers = api.NewAsyncHandler(assistantService, authService, repo, cfg)
		quota := cfg.Async.WithQuotaDefaults()
		limit := execution.QuotaLimit{Concurrency: quota.Concurrency, RPM: quota.RPM, TPM: quota.TPM}
		runtime = &execution.Runtime{Repo: repo, Redis: rdb.Raw(), Provider: execution.NewHTTPProvider(timeout), Decrypt: assistantService.OpenExecution, Workers: cfg.Async.Workers, Quota: execution.QuotaPolicy{Platform: limit, Credential: limit, User: execution.QuotaLimit{Concurrency: quota.UserConcurrency, RPM: quota.RPM, TPM: quota.TPM}, Grace: time.Minute}}
		if err := repo.BindPolicy(rootCtx, runtime.Quota); err != nil {
			log.Fatalf("quota configuration: %v", err)
		}
		fingerprint, err := assistant.ExecutionKeyFingerprint()
		if err != nil {
			log.Fatal("invalid execution encryption key")
		}
		if err = repo.BindEncryptionKey(rootCtx, fingerprint); err != nil {
			log.Fatalf("execution encryption key: %v", err)
		}
		type routingConfig struct {
			BaseURL    string `json:"base_url"`
			QuotaGroup string `json:"quota_group"`
		}
		routing := make(map[string]routingConfig, len(cfg.Providers))
		for name, p := range cfg.Providers {
			routing[name] = routingConfig{BaseURL: p.BaseURL, QuotaGroup: p.QuotaGroup}
		}
		routingJSON, err := json.Marshal(routing)
		if err != nil {
			log.Fatal("invalid provider routing configuration")
		}
		routingHash := sha256.Sum256(routingJSON)
		if err = repo.BindProviderRouting(rootCtx, hex.EncodeToString(routingHash[:])); err != nil {
			log.Fatalf("provider routing: %v", err)
		}
		if role != "api" {
			go func() { defer close(done); runtime.Run(rootCtx) }()
		} else {
			close(done)
		}
		if role == "worker" {
			workerOnly = true
		}
	} else {
		handlers = api.NewHandler(assistantService, authService, workerCfg, fileBase, tempTTL, rdb)
		close(done)
	}

	router := gin.Default()
	if !workerOnly {
		handlers.RegisterRoutes(router)
	}
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if runtime != nil {
			if err := runtime.Ready(ctx); err != nil {
				c.JSON(503, gin.H{"ready": false})
				return
			}
		}
		c.JSON(200, gin.H{"ready": true})
	})
	if runtime != nil {
		router.GET("/internal/execution/metrics", func(c *gin.Context) {
			secret := os.Getenv("UNICHATGO_OPERATIONS_TOKEN")
			if secret == "" || subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+secret)) != 1 {
				c.AbortWithStatus(404)
				return
			}
			ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			defer cancel()
			metrics, err := runtime.Metrics(ctx)
			if err != nil {
				c.JSON(503, gin.H{"error": "metrics_unavailable"})
				return
			}
			c.JSON(200, metrics)
		})
	}

	addr := cfg.BasicConfig.ServerAddress
	if addr == "" {
		addr = ":8090"
	}

	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-rootCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("server stopped: %v", err)
		stop()
	}
	<-done
}
