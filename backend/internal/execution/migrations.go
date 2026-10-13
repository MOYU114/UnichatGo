package execution

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Migrate is additive and rerunnable after interruption between MySQL DDL statements.
// The advisory lock belongs to this connection, not the connection pool.
func Migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked int
	if err = conn.QueryRowContext(ctx, `SELECT GET_LOCK(CONCAT('execution:',LEFT(SHA2(DATABASE(),256),48)), 30)`).Scan(&locked); err != nil {
		return err
	}
	if locked != 1 {
		return fmt.Errorf("execution migration lock unavailable")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(releaseCtx, `SELECT RELEASE_LOCK(CONCAT('execution:',LEFT(SHA2(DATABASE(),256),48)))`)
	}()
	var exists int
	if err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='execution_schema_versions'`).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		var version sql.NullInt64
		if err = conn.QueryRowContext(ctx, `SELECT MAX(version) FROM execution_schema_versions`).Scan(&version); err != nil {
			return err
		}
		if version.Valid && version.Int64 > 1 {
			return fmt.Errorf("execution schema %d is newer than this binary", version.Int64)
		}
	}
	for _, stmt := range schema {
		if _, err = conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("execution migration: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, `INSERT IGNORE INTO execution_schema_versions(version,applied_at) VALUES(1,NOW(3))`)
	return err
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS execution_schema_versions(version INT PRIMARY KEY,applied_at DATETIME(3) NOT NULL) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_control(id INT PRIMARY KEY,async_enabled BOOLEAN NOT NULL) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_policy(name VARCHAR(32) CHARACTER SET ascii PRIMARY KEY,fingerprint CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`,
	`INSERT IGNORE INTO execution_control(id,async_enabled) VALUES(1,TRUE)`,
	`CREATE TABLE IF NOT EXISTS execution_quarantine(stream_id VARBINARY(128) PRIMARY KEY,reason VARCHAR(255) NOT NULL,created_ms BIGINT NOT NULL) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_admission_events(id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,user_id BIGINT UNSIGNED NOT NULL,created_ms BIGINT NOT NULL,INDEX execution_admission_window(user_id,created_ms),FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_sessions(
	 session_id BIGINT UNSIGNED PRIMARY KEY, next_sequence BIGINT NOT NULL DEFAULT 1,
	 completed_sequence BIGINT NOT NULL DEFAULT 0, legacy_max_message_id BIGINT NOT NULL,
	 FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_evidence(
	 id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
	 user_id BIGINT UNSIGNED NOT NULL, session_id BIGINT UNSIGNED NOT NULL,
	 client_id VARBINARY(128) NOT NULL,payload_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	 user_message_id BIGINT UNSIGNED NOT NULL,assistant_message_id BIGINT UNSIGNED NULL,
	 sequence BIGINT NOT NULL,provider VARCHAR(100) NOT NULL,model VARCHAR(255) NOT NULL,
	 snapshot MEDIUMTEXT NOT NULL,quota_scope VARBINARY(255) NOT NULL,
	 attachment_ids TEXT NOT NULL,state VARCHAR(20) NOT NULL,error_code VARCHAR(100) NOT NULL DEFAULT '',
	 owner VARBINARY(128) NOT NULL DEFAULT '',fence BIGINT NOT NULL DEFAULT 0,
	 lease_until_ms BIGINT NOT NULL DEFAULT 0,call_started BOOLEAN NOT NULL DEFAULT FALSE,
	 created_ms BIGINT NOT NULL,deadline_ms BIGINT NOT NULL,
	 UNIQUE KEY execution_client(user_id,session_id,client_id),UNIQUE KEY execution_sequence(session_id,sequence),
	 UNIQUE KEY execution_user_message(user_message_id), UNIQUE KEY execution_assistant_message(assistant_message_id),
	 INDEX execution_recovery(state,lease_until_ms), INDEX execution_user_state(user_id,state),
	 INDEX execution_user_created(user_id,created_ms),
	 FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
	 FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE,
	 FOREIGN KEY(user_message_id) REFERENCES messages(id) ON DELETE CASCADE,
	 FOREIGN KEY(assistant_message_id) REFERENCES messages(id) ON DELETE SET NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS execution_outbox(
	 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
	 generation BIGINT NOT NULL DEFAULT 1,next_publish_ms BIGINT NOT NULL,publish_count BIGINT NOT NULL DEFAULT 0,
	 INDEX execution_outbox_due(next_publish_ms),
	 FOREIGN KEY(request_id) REFERENCES execution_evidence(id) ON DELETE CASCADE) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_quota_scopes(scope VARBINARY(255) PRIMARY KEY) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_quota_calls(
	 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,scope VARBINARY(255) NOT NULL,
	 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,fence BIGINT NOT NULL,
	 tokens BIGINT NOT NULL,created_ms BIGINT NOT NULL,hold_until_ms BIGINT NOT NULL,
	 INDEX execution_quota_window(scope,created_ms),INDEX execution_quota_holds(scope,hold_until_ms),
	 INDEX execution_quota_request(request_id,fence)) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_audit(
	 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	 actor_id BIGINT UNSIGNED NOT NULL,old_fence BIGINT NOT NULL,action VARCHAR(32) NOT NULL,
	 reason VARCHAR(1000) NOT NULL,created_ms BIGINT NOT NULL,
	 FOREIGN KEY(request_id) REFERENCES execution_evidence(id) ON DELETE CASCADE) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS execution_attachments(
	 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,user_id BIGINT UNSIGNED NOT NULL,session_id BIGINT UNSIGNED NOT NULL,
	 name VARCHAR(255) NOT NULL,mime VARCHAR(255) NOT NULL,size BIGINT NOT NULL,
	 sha256 CHAR(64) CHARACTER SET ascii NOT NULL, data MEDIUMBLOB NOT NULL,
	 created_ms BIGINT NOT NULL,expires_ms BIGINT NOT NULL,
	 INDEX execution_attachment_user(user_id),INDEX execution_attachment_expiry(expires_ms),
	 FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
	 FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS execution_attachment_pins(
	 request_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,attachment_id BIGINT UNSIGNED NOT NULL,
	 PRIMARY KEY(request_id,attachment_id),
	 FOREIGN KEY(request_id) REFERENCES execution_evidence(id) ON DELETE CASCADE,
	 FOREIGN KEY(attachment_id) REFERENCES execution_attachments(id)) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS execution_legacy_attachments(
	 legacy_id BIGINT UNSIGNED PRIMARY KEY,attachment_id BIGINT UNSIGNED UNIQUE NOT NULL,
	 source_sha256 CHAR(64) CHARACTER SET ascii NOT NULL,migrated_ms BIGINT NOT NULL,
	 FOREIGN KEY(legacy_id) REFERENCES temp_files(id) ON DELETE CASCADE) ENGINE=InnoDB`,
}

func (r *Repository) CheckSchema(ctx context.Context) error {
	var version int
	if err := r.DB.QueryRowContext(ctx, `SELECT MAX(version) FROM execution_schema_versions`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported execution schema version %d", version)
	}
	var enabled bool
	if err := r.DB.QueryRowContext(ctx, `SELECT async_enabled FROM execution_control WHERE id=1`).Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return fmt.Errorf("async execution write gate is disabled")
	}
	return nil
}

func (r *Repository) Quarantine(ctx context.Context, streamID, reason string) error {
	if len(streamID) == 0 || len(streamID) > 128 || len(reason) > 255 {
		return fmt.Errorf("invalid quarantine metadata")
	}
	_, err := r.DB.ExecContext(ctx, `INSERT IGNORE INTO execution_quarantine(stream_id,reason,created_ms) VALUES(?,?,`+sqlNow+`)`, []byte(streamID), reason)
	return err
}
