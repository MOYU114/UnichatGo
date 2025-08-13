package storage

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Open connects to the SQLite database at the provided path.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		path = "app.db"
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// open SQLite PRAGMA, enabling foreign-key constraints
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// Migrate ensures the required tables are present.
func Migrate(db *sql.DB) error {
	stmts := []string{
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
		`CREATE INDEX IF NOT EXISTS idx_messages_user ON messages(user_id);
		 CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);`,
	}

	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
