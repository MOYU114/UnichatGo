package execution

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"unichatgo/internal/storage"
)

func isolatedSchema(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("UNICHATGO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("real MySQL not executed: UNICHATGO_TEST_MYSQL_DSN unset")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "execution_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE DATABASE ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg.DBName = schema
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP DATABASE ` + schema); admin.Close() })
	if err = storage.Migrate(db, "mysql"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMySQLMigrationGateAndFutureVersion(t *testing.T) {
	db := isolatedSchema(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewRepository(db, Limits{})
	if _, err := db.Exec(`UPDATE execution_control SET async_enabled=FALSE WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckSchema(ctx); err == nil {
		t.Fatal("migration overwrote disabled gate")
	}
	if _, err := db.Exec(`INSERT INTO execution_schema_versions(version,applied_at) VALUES(99,NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE execution_policy`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err == nil {
		t.Fatal("older binary accepted future schema")
	}
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='execution_policy'`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("migration wrote schema before rejecting future version")
	}
}

func TestMySQLDeploymentPolicyBinding(t *testing.T) {
	db := isolatedSchema(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewRepository(db, Limits{})
	policy := testPolicy()
	if err := r.BindPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := NewRepository(db, Limits{}).BindPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	changed := policy
	changed.Platform.Concurrency++
	if err := r.BindPolicy(ctx, changed); err == nil {
		t.Fatal("mixed global quota policy allowed")
	}
	key := HashPayload([]byte("isolated-test-key"))
	if err := r.BindEncryptionKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.BindEncryptionKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.BindEncryptionKey(ctx, HashPayload([]byte("wrong-key"))); err == nil {
		t.Fatal("wrong encryption key allowed")
	}
	routing := HashPayload([]byte(`{"openai":{"base_url":"https://mock.invalid","quota_group":"shared-account"}}`))
	if err := r.BindProviderRouting(ctx, routing); err != nil {
		t.Fatal(err)
	}
	if err := r.BindProviderRouting(ctx, routing); err != nil {
		t.Fatal(err)
	}
	if err := r.BindProviderRouting(ctx, HashPayload([]byte("different-account-group"))); err == nil {
		t.Fatal("mixed provider account grouping allowed")
	}
	tx, err := r.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = checkPolicy(ctx, tx, changed); err == nil {
		t.Fatal("call bypassed bound quota policy")
	}
}

func TestMySQLAdmissionPolicyBinding(t *testing.T) {
	db := isolatedSchema(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	r := NewRepository(db, Limits{})
	if err := r.BindAdmission(ctx); err != nil {
		t.Fatal(err)
	}
	if err := NewRepository(db, Limits{}).BindAdmission(ctx); err != nil {
		t.Fatal(err)
	}
	user, err := db.Exec(`INSERT INTO users(username,password_hash,created_at) VALUES('admission-policy','isolated',NOW())`)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := user.LastInsertId()
	sid := testSession(t, r, uid)
	in := testInput(uid, sid, "original", "accepted")
	receipt := acceptTest(t, r, in)
	for _, dimension := range []string{"session", "user", "rpm", "deadline"} {
		changed := r.limits
		switch dimension {
		case "session":
			changed.SessionQueue++
		case "user":
			changed.UserQueue++
		case "rpm":
			changed.AcceptRPM++
		case "deadline":
			changed.TaskTimeout += time.Second
		}
		other := NewRepository(db, changed)
		if err := other.BindAdmission(ctx); err == nil {
			t.Fatalf("mixed %s policy allowed", dimension)
		}
		if _, _, err := other.Accept(ctx, testInput(uid, sid, "new-"+dimension, "rejected")); err == nil {
			t.Fatalf("mixed %s instance accepted", dimension)
		}
		got, replay, err := other.Accept(ctx, in)
		if err != nil || !replay || got.ID != receipt.ID {
			t.Fatalf("replay depended on mutable admission %s: %+v %v %v", dimension, got, replay, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM execution_evidence`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("mismatch wrote partial request: %d %v", count, err)
	}
}
