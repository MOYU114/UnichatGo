package main

import (
	"log"
	"os"
	"time"

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

	db, err := storage.Open(cfg.BasicConfig.DatabasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	// Create necessary tables: users, apiKeys, sessions, messages
	if err := storage.Migrate(db); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	//TODO: aiService need to add in the future

	assistantService, err := assistant.NewService(db)
	if err != nil {
		log.Fatalf("init assistant service: %v", err)
	}
	authService := auth.NewService(db, 24*time.Hour)
	handlers := api.NewHandler(assistantService, authService)

	router := gin.Default()
	handlers.RegisterRoutes(router)

	addr := cfg.BasicConfig.ServerAddress
	if addr == "" {
		addr = ":8080"
	}

	if err := router.Run(addr); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
