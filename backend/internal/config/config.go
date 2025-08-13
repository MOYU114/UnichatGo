package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents runtime configuration for the service.
type Config struct {
	AI          AIConfig        `json:"ai"`
	Assistant   AssistantConfig `json:"assistant"`
	BasicConfig BasicConfig     `json:"basic_config"`
}

type AIConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIToken string `json:"api_token"`
}
type AssistantConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIToken string `json:"api_token"`
}

type BasicConfig struct {
	ServerAddress string `json:"server_address"`
	DatabasePath  string `json:"database_path"`
}

// Load reads configuration from the provided path (defaults to config.json).
func Load(path string) (*Config, error) {
	if path == "" {
		path = "config.json"
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}

	file, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", absPath, err)
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if cfg.BasicConfig.DatabasePath == "" {
		return nil, fmt.Errorf("database_path must be configured")
	}

	if !filepath.IsAbs(cfg.BasicConfig.DatabasePath) {
		cfg.BasicConfig.DatabasePath = filepath.Join(filepath.Dir(absPath), cfg.BasicConfig.DatabasePath)
	}

	return &cfg, nil
}
