package main

import (
	"log"
	"os"
	"time"
	"unichatgo/internal/worker"

	"github.com/gin-gonic/gin"

	"unichatgo/internal/api"
	"unichatgo/internal/auth"
	"unichatgo/internal/config"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/storage"
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
	// Create necessary tables: users, apiKeys, sessions, messages
	if err := storage.Migrate(db, dbType); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	assistantService, err := assistant.NewService(db)
	if err != nil {
		log.Fatalf("init assistant service: %v", err)
	}
	workerCfg := worker.DispatcherConfig{MaxWorkers: cfg.BasicConfig.MaxWorkers, QueueSize: cfg.BasicConfig.QueueSize}
	authService := auth.NewService(db, 24*time.Hour)
	handlers := api.NewHandler(assistantService, authService, workerCfg)

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
