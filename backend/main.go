package main

import (
	"context"
	"log"
	"os"
	"time"

	"unichatgo/internal/api"
	"unichatgo/internal/auth"
	"unichatgo/internal/config"
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
	rdb, err := redis.NewRedisClient(cfg)
	if err != nil {
		log.Fatalf("create redis client: %v", err)
	}
	defer rdb.Close()

	// Create necessary tables: users, apiKeys, sessions, messages
	if err := storage.Migrate(db, dbType); err != nil {
		log.Fatalf("migrate database: %v", err)
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
	assistantService.StartTempFileCleaner(cleanCtx, cleanInterval)
	authService := auth.NewService(db, rdb, 24*time.Hour)
	fileBase := cfg.BasicConfig.FileBaseDir
	if fileBase == "" {
		fileBase = "./data/uploads"
	}
	tempTTL := time.Duration(cfg.BasicConfig.TempFileTTL) * time.Minute
	if tempTTL <= 0 {
		tempTTL = assistant.DefaultTempFileTTL
	}
	handlers := api.NewHandler(assistantService, authService, workerCfg, fileBase, tempTTL, rdb)

	router := gin.Default()
	handlers.RegisterRoutes(router)

	addr := cfg.BasicConfig.ServerAddress
	if addr == "" {
		addr = ":8090"
	}

	if err := router.Run(addr); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
