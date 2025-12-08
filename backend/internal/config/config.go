package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents runtime configuration for the service.
type Config struct {
	BasicConfig BasicConfig               `json:"basic_config"`
	Providers   map[string]ProviderConfig `json:"providers"`
	Databases   map[string]DatabaseConfig `json:"databases"`
}

type DatabaseConfig struct {
	// for sqlite3
	DSN string `json:"dsn"`
	// for mysql only
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	DBName   string `json:"db_name"`
	Params   string `json:"params"`
}
type ProviderConfig struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
}

type BasicConfig struct {
	ServerAddress string `json:"server_address"`
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

	return &cfg, nil
}
