package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func legacyFixture(t *testing.T, r *Repository, uid, sid int64, path string, data []byte) int64 {
	t.Helper()
	res, err := r.DB.Exec(`INSERT INTO temp_files(user_id,session_id,file_name,stored_path,mime_type,size,status,created_at,expires_at) VALUES(?,?,'legacy.txt',?,'text/plain',?,'active',NOW(),DATE_ADD(NOW(),INTERVAL 1 HOUR))`, uid, sid, path, len(data))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLegacySourceBoundary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "uploads")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("allowed")
	if err := os.WriteFile(filepath.Join(root, "good.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "outside.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside.txt", filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("good.txt", filepath.Join(root, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewLegacyMigrator(nil, root, base)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, path := range []string{"uploads/good.txt", filepath.Join(root, "good.txt"), "uploads/inside.txt"} {
		got, err := m.read(LegacyAttachment{Path: path, Size: int64(len(data))})
		if err != nil || string(got) != string(data) {
			t.Fatalf("allowed source %q: %q %v", path, got, err)
		}
	}
	for _, path := range []string{"outside.txt", "uploads/../outside.txt", "uploads/escape.txt", filepath.Join(base, "outside.txt"), "uploads/missing.txt", "uploads/pipe"} {
		if _, err := m.read(LegacyAttachment{Path: path, Size: int64(len(data))}); err == nil {
			t.Fatalf("escaped/missing source accepted: %q", path)
		}
	}
	if _, err := m.read(LegacyAttachment{Path: "uploads/good.txt", Size: MaxAttachmentBytes + 1}); err == nil {
		t.Fatal("oversized metadata accepted")
	}
}

func TestMySQLLegacyCopyDryRunVerifyAndChangedSource(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "legacy.txt")
	data := []byte("immutable original")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	id := legacyFixture(t, r, uid, sid, path, data)
	m, err := NewLegacyMigrator(r, root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	dry, err := m.Copy(ctx, id, false)
	if err != nil || dry.State != "would_copy" || dry.AttachmentID != 0 {
		t.Fatalf("dry=%+v %v", dry, err)
	}
	var count int
	if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_attachments WHERE user_id=?`, uid).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry-run wrote data: %d %v", count, err)
	}
	first, err := m.Copy(ctx, id, true)
	if err != nil || first.State != "copied" || !first.Executable {
		t.Fatalf("first=%+v %v", first, err)
	}
	loaded, err := NewRepository(r.DB, r.limits).LoadAttachment(ctx, uid, sid, first.AttachmentID)
	if err != nil || string(loaded.Data) != string(data) || loaded.SHA256 != HashPayload(data) {
		t.Fatalf("shared object=%+v %v", loaded, err)
	}
	var expiry, legacyExpiry int64
	if err = r.DB.QueryRow(`SELECT expires_ms FROM execution_attachments WHERE id=?`, first.AttachmentID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if err = r.DB.QueryRow(`SELECT CAST(UNIX_TIMESTAMP(expires_at)*1000 AS SIGNED) FROM temp_files WHERE id=?`, id).Scan(&legacyExpiry); err != nil || expiry != legacyExpiry {
		t.Fatalf("expiry changed %d %d %v", expiry, legacyExpiry, err)
	}
	second, err := m.Copy(ctx, id, true)
	if err != nil || second.State != "verified" || second.AttachmentID != first.AttachmentID {
		t.Fatalf("rerun=%+v %v", second, err)
	}
	if err = r.DB.QueryRow(`SELECT COUNT(*) FROM execution_attachments WHERE user_id=?`, uid).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rerun duplicated object: %d %v", count, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("original changed: %q %v", got, err)
	}
	if err = os.WriteFile(path, []byte(strings.Repeat("x", len(data))), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Copy(ctx, id, true); err == nil || !strings.Contains(err.Error(), "hash_changed") {
		t.Fatalf("changed original accepted: %v", err)
	}
}

func TestMySQLLegacyMissingMappedObjectAndExpired(t *testing.T) {
	r, uid, sid := mysqlRepository(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "legacy.txt")
	data := []byte("source")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	id := legacyFixture(t, r, uid, sid, path, data)
	m, err := NewLegacyMigrator(r, root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	first, err := m.Copy(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.DeleteAttachment(ctx, uid, sid, first.AttachmentID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Copy(ctx, id, true); err == nil || !strings.Contains(err.Error(), "mapped_object_missing") {
		t.Fatalf("missing object silently recopied: %v", err)
	}
	expired := legacyFixture(t, r, uid, sid, filepath.Join(root, "does-not-exist"), data)
	if _, err = r.DB.Exec(`UPDATE temp_files SET expires_at=DATE_SUB(NOW(),INTERVAL 1 SECOND) WHERE id=?`, expired); err != nil {
		t.Fatal(err)
	}
	result, err := m.Copy(ctx, expired, true)
	if err != nil || result.State != "expired" {
		t.Fatalf("expired not reported: %+v %v", result, err)
	}
	missing := legacyFixture(t, r, uid, sid, filepath.Join(root, "missing"), data)
	if _, err = m.Copy(ctx, missing, true); err == nil || !strings.Contains(err.Error(), "source_unavailable") {
		t.Fatalf("missing not reported: %v", err)
	}
}
