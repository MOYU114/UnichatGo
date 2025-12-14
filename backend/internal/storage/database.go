package storage

import (
	"database/sql"
	"fmt"
	"strings"

	"unichatgo/internal/config"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
)

// Open connects to the SQLite database at the provided path.
func Open(dbType string, cfg *config.Config) (*sql.DB, error) {
	dbCfg, ok := cfg.Databases[dbType]
	if !ok {
		return nil, fmt.Errorf("database config for %s not found", dbType)
	}

	var (
		db  *sql.DB
		err error
	)

	switch strings.ToLower(dbType) {
	case "sqlite", "sqlite3":
		if dbCfg.DSN == "" {
			return nil, fmt.Errorf("sqlite dsn must be provided")
		}
		db, err = sql.Open("sqlite3", dbCfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("open sqlite database: %w", err)
		}
		if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
			db.Close()
			return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
		}
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?%s",
			dbCfg.Username,
			dbCfg.Password,
			dbCfg.Host,
			dbCfg.Port,
			dbCfg.DBName,
			dbCfg.Params,
		)
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			return nil, fmt.Errorf("open mysql database: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported driver: %s", dbType)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// Migrate ensures the required tables are present.
func Migrate(db *sql.DB, driver string) error {
	var stmts []string
	switch strings.ToLower(driver) {
	case "sqlite", "sqlite3":
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS users (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				username TEXT NOT NULL UNIQUE,
				password_hash TEXT NOT NULL,
				created_at DATETIME NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS sessions (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL,
				title TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				updated_at DATETIME NOT NULL,
				FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
			)`,
			`CREATE TABLE IF NOT EXISTS messages (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL,
				session_id INTEGER NOT NULL,
				role TEXT NOT NULL,
				content TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
				FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
			)`,
			`CREATE TABLE IF NOT EXISTS apiKeys (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL,
				provider TEXT NOT NULL,
				api_key TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				UNIQUE(user_id, provider),
				FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
			)`,
			`CREATE TABLE IF NOT EXISTS user_tokens (
				token TEXT PRIMARY KEY,
				user_id INTEGER NOT NULL,
				created_at DATETIME NOT NULL,
				expires_at DATETIME NOT NULL,
				FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_user_tokens_user ON user_tokens(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_user ON messages(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC)`,
			`CREATE TABLE IF NOT EXISTS temp_files (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL,
				session_id INTEGER NOT NULL,
				file_name TEXT NOT NULL,
				stored_path TEXT NOT NULL,
				mime_type TEXT NOT NULL,
				size INTEGER NOT NULL,
				status TEXT NOT NULL DEFAULT 'active',
				summary TEXT,
				summary_message_id INTEGER,
				created_at DATETIME NOT NULL,
				expires_at DATETIME NOT NULL,
				FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
				FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE,
				FOREIGN KEY(summary_message_id) REFERENCES messages(id) ON DELETE SET NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_temp_files_user ON temp_files(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_temp_files_expiry ON temp_files(expires_at)`,
		}
	case "mysql":
		stmts = []string{
			`CREATE TABLE IF NOT EXISTS users (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				username VARCHAR(255) NOT NULL UNIQUE,
				password_hash VARCHAR(255) NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY (id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS sessions (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				user_id BIGINT UNSIGNED NOT NULL,
				title VARCHAR(255) NOT NULL,
				created_at DATETIME NOT NULL,
				updated_at DATETIME NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_sessions_user (user_id),
				INDEX idx_sessions_updated_at (updated_at),
				CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS messages (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				user_id BIGINT UNSIGNED NOT NULL,
				session_id BIGINT UNSIGNED NOT NULL,
				role VARCHAR(50) NOT NULL,
				content MEDIUMTEXT NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_messages_user (user_id),
				INDEX idx_messages_session (session_id),
				CONSTRAINT fk_messages_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
				CONSTRAINT fk_messages_session FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS apiKeys (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				user_id BIGINT UNSIGNED NOT NULL,
				provider VARCHAR(100) NOT NULL,
				api_key TEXT NOT NULL,
				created_at DATETIME NOT NULL,
				PRIMARY KEY (id),
				UNIQUE KEY uniq_user_provider (user_id, provider),
				CONSTRAINT fk_apikeys_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS user_tokens (
				token VARCHAR(255) NOT NULL PRIMARY KEY,
				user_id BIGINT UNSIGNED NOT NULL,
				created_at DATETIME NOT NULL,
				expires_at DATETIME NOT NULL,
				INDEX idx_user_tokens_user (user_id),
				CONSTRAINT fk_user_tokens_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS temp_files (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				user_id BIGINT UNSIGNED NOT NULL,
				session_id BIGINT UNSIGNED NOT NULL,
				file_name VARCHAR(255) NOT NULL,
				stored_path TEXT NOT NULL,
				mime_type VARCHAR(255) NOT NULL,
				size BIGINT NOT NULL,
				status VARCHAR(50) NOT NULL DEFAULT 'active',
				summary MEDIUMTEXT,
				summary_message_id BIGINT UNSIGNED,
				created_at DATETIME NOT NULL,
				expires_at DATETIME NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_temp_files_user (user_id),
				INDEX idx_temp_files_expiry (expires_at),
				CONSTRAINT fk_temp_files_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
				CONSTRAINT fk_temp_files_session FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
				CONSTRAINT fk_temp_files_summary_msg FOREIGN KEY (summary_message_id) REFERENCES messages(id) ON DELETE SET NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		}
	default:
		return fmt.Errorf("unsupported driver for migration: %s", driver)
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate (%s): %w", driver, err)
		}
	}
	return nil
}
