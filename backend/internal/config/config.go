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
	Redis       RedisConfig               `json:"redis"`
	Async       AsyncConfig               `json:"async"`
}

// Async is opt-in; one deployment must never mix legacy and async writers.
type AsyncConfig struct {
	Enabled            bool   `json:"enabled"`
	Role               string `json:"role"`
	Paused             bool   `json:"paused"`
	Workers            int    `json:"workers"`
	SessionQueue       int    `json:"session_queue"`
	UserQueue          int    `json:"user_queue"`
	AcceptRPM          int    `json:"accept_rpm"`
	TaskTimeoutSeconds int    `json:"task_timeout_seconds"`
	Concurrency        int    `json:"concurrency"`
	RPM                int    `json:"rpm"`
	TPM                int64  `json:"tpm"`
	UserConcurrency    int    `json:"user_concurrency"`
}

// WithQuotaDefaults is shared by admission preflight and worker startup.
// It does not alter explicitly configured limits or increase limits for images.
func (cfg AsyncConfig) WithQuotaDefaults() AsyncConfig {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.RPM <= 0 {
		cfg.RPM = 60
	}
	if cfg.TPM <= 0 {
		cfg.TPM = 200000
	}
	if cfg.UserConcurrency <= 0 {
		cfg.UserConcurrency = 2
	}
	return cfg
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
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key"`
	QuotaGroup string `json:"quota_group,omitempty"`
}

type BasicConfig struct {
	ServerAddress     string `json:"server_address"`
	MinWorkers        int    `json:"min_workers"`
	MaxWorkers        int    `json:"max_workers"`
	QueueSize         int    `json:"queue_size"`
	WorkerIdleTimeout int    `json:"worker_idle_timeout_minutes"`
	FileBaseDir       string `json:"file_base_dir"`
	TempFileTTL       int    `json:"temp_file_ttl_minutes"`
	TempCleanInterval int    `json:"temp_file_clean_interval_minutes"`
}

type RedisConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	DB       int    `json:"db_name"`
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
