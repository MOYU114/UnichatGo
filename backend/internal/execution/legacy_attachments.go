package execution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type LegacyAttachment struct {
	ID, UserID, SessionID, Size, CreatedMS, ExpiresMS int64
	Name, Path, MIME, Status                          string
}

type LegacyMigrationResult struct {
	LegacyID     int64  `json:"legacy_id"`
	AttachmentID int64  `json:"attachment_id,omitempty"`
	State        string `json:"state"`
	SHA256       string `json:"sha256,omitempty"`
	Executable   bool   `json:"executable"`
}

const legacyColumns = `id,user_id,session_id,file_name,stored_path,mime_type,size,status,CAST(UNIX_TIMESTAMP(created_at)*1000 AS SIGNED),CAST(UNIX_TIMESTAMP(expires_at)*1000 AS SIGNED)`

func scanLegacy(s scanner) (LegacyAttachment, error) {
	var a LegacyAttachment
	err := s.Scan(&a.ID, &a.UserID, &a.SessionID, &a.Name, &a.Path, &a.MIME, &a.Size, &a.Status, &a.CreatedMS, &a.ExpiresMS)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}

func (r *Repository) LegacyAttachments(ctx context.Context, after int64, limit int) ([]LegacyAttachment, error) {
	rows, err := r.DB.QueryContext(ctx, `SELECT `+legacyColumns+` FROM temp_files WHERE id>? ORDER BY id LIMIT ?`, after, min(max(limit, 1), 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []LegacyAttachment
	for rows.Next() {
		a, err := scanLegacy(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, a)
	}
	return files, rows.Err()
}

// LegacyMigrator copies only explicitly selected source-host files. os.Root
// enforces the directory boundary even if a symlink is swapped while reading.
// SourceBase defines how legacy relative stored_path values were originally resolved.
type LegacyMigrator struct {
	repo       *Repository
	root       *os.Root
	rootPath   string
	sourceBase string
}

func NewLegacyMigrator(repo *Repository, rootPath, sourceBase string) (*LegacyMigrator, error) {
	if rootPath == "" || sourceBase == "" {
		return nil, errors.New("explicit root and source-base are required")
	}
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}
	sourceBase, err = filepath.Abs(sourceBase)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	return &LegacyMigrator{repo: repo, root: root, rootPath: rootPath, sourceBase: sourceBase}, nil
}

func (m *LegacyMigrator) Close() error { return m.root.Close() }

func (m *LegacyMigrator) read(a LegacyAttachment) ([]byte, error) {
	if a.Size <= 0 || a.Size > MaxAttachmentBytes {
		return nil, errors.New("legacy_size_invalid")
	}
	path := a.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.sourceBase, path)
	}
	rel, err := filepath.Rel(m.rootPath, filepath.Clean(path))
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return nil, errors.New("legacy_path_outside_root")
	}
	// Nonblocking open avoids hanging if an unexpected FIFO replaces a source
	// between path selection and fstat; only regular files pass below.
	file, err := m.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("legacy_source_unavailable")
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != a.Size {
		return nil, errors.New("legacy_source_metadata_changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxAttachmentBytes+1))
	if err != nil || int64(len(data)) != a.Size {
		return nil, errors.New("legacy_source_read_failed")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("legacy_source_changed")
	}
	return data, nil
}

func (m *LegacyMigrator) Copy(ctx context.Context, legacyID int64, apply bool) (LegacyMigrationResult, error) {
	result := LegacyMigrationResult{LegacyID: legacyID}
	a, err := scanLegacy(m.repo.DB.QueryRowContext(ctx, `SELECT `+legacyColumns+` FROM temp_files WHERE id=?`, legacyID))
	if err != nil {
		return result, err
	}
	var now int64
	if err = m.repo.DB.QueryRowContext(ctx, `SELECT `+sqlNow).Scan(&now); err != nil {
		return result, err
	}
	if a.Status != "active" {
		result.State = "inactive"
		return result, nil
	}
	if a.ExpiresMS <= now {
		result.State = "expired"
		return result, nil
	}
	if len(a.Name) == 0 || len(a.Name) > 255 || len(a.MIME) > 255 || strings.ContainsAny(a.Name, "\x00\r\n") {
		return result, errors.New("legacy_metadata_invalid")
	}
	data, err := m.read(a)
	if err != nil {
		return result, err
	}
	result.SHA256 = HashPayload(data)
	result.Executable = ValidateAttachments([]Attachment{{Name: a.Name, MIME: a.MIME, Size: a.Size, Data: data}}) == nil
	tx, err := m.repo.begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err = lockOwned(ctx, tx, a.UserID, a.SessionID); err != nil {
		return result, err
	}
	current, err := scanLegacy(tx.QueryRowContext(ctx, `SELECT `+legacyColumns+` FROM temp_files WHERE id=? FOR UPDATE`, legacyID))
	if err != nil {
		return result, err
	}
	if current != a {
		return result, errors.New("legacy_metadata_changed")
	}
	now, err = nowMS(ctx, tx)
	if err != nil {
		return result, err
	}
	if a.ExpiresMS <= now {
		result.State = "expired"
		return result, nil
	}
	var mappedID int64
	var mappedHash string
	err = tx.QueryRowContext(ctx, `SELECT attachment_id,source_sha256 FROM execution_legacy_attachments WHERE legacy_id=? FOR UPDATE`, legacyID).Scan(&mappedID, &mappedHash)
	if err == nil {
		if mappedHash != result.SHA256 {
			return result, errors.New("legacy_source_hash_changed")
		}
		copy, err := scanAttachment(tx.QueryRowContext(ctx, `SELECT `+attachmentColumns+`,data FROM execution_attachments WHERE id=? AND user_id=? AND session_id=?`, mappedID, a.UserID, a.SessionID), true)
		if err != nil {
			return result, errors.New("legacy_mapped_object_missing")
		}
		if copy.SHA256 != mappedHash || HashPayload(copy.Data) != mappedHash || copy.Size != a.Size || copy.Name != a.Name || copy.MIME != a.MIME {
			return result, errors.New("legacy_mapped_object_changed")
		}
		result.AttachmentID, result.State = mappedID, "verified"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	} else {
		var used int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM execution_attachments WHERE user_id=?`, a.UserID).Scan(&used); err != nil {
			return result, err
		}
		if used+a.Size > MaxUserAttachmentBytes {
			return result, ErrLimit
		}
		result.State = "would_copy"
		if apply {
			res, err := tx.ExecContext(ctx, `INSERT INTO execution_attachments(user_id,session_id,name,mime,size,sha256,data,created_ms,expires_ms) VALUES(?,?,?,?,?,?,?,?,?)`, a.UserID, a.SessionID, a.Name, a.MIME, a.Size, result.SHA256, data, a.CreatedMS, a.ExpiresMS)
			if err != nil {
				return result, err
			}
			result.AttachmentID, err = res.LastInsertId()
			if err != nil {
				return result, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO execution_legacy_attachments(legacy_id,attachment_id,source_sha256,migrated_ms) VALUES(?,?,?,?)`, legacyID, result.AttachmentID, result.SHA256, now); err != nil {
				return result, err
			}
			var stored []byte
			if err = tx.QueryRowContext(ctx, `SELECT data FROM execution_attachments WHERE id=?`, result.AttachmentID).Scan(&stored); err != nil {
				return result, err
			}
			if HashPayload(stored) != result.SHA256 {
				return result, errors.New("legacy_copy_verification_failed")
			}
			result.State = "copied"
		}
	}
	// Verify source again before committing both object and mapping. Original
	// metadata/files are never changed or deleted, even after successful copy.
	verify, err := m.read(a)
	if err != nil {
		return result, err
	}
	if HashPayload(verify) != result.SHA256 {
		return result, errors.New("legacy_source_changed")
	}
	if apply {
		err = tx.Commit()
	}
	return result, err
}

func (r LegacyMigrationResult) String() string {
	return fmt.Sprintf("legacy=%d attachment=%d state=%s", r.LegacyID, r.AttachmentID, r.State)
}
