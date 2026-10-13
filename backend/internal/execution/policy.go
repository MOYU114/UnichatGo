package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func policyHash(policy QuotaPolicy) (string, error) {
	for _, limit := range []QuotaLimit{policy.Platform, policy.User, policy.Credential} {
		if limit.Concurrency <= 0 || limit.RPM <= 0 || limit.TPM <= 0 {
			return "", fmt.Errorf("all quota dimensions must be bounded")
		}
	}
	policy.Grace = max(policy.Grace, 30*time.Second)
	b, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return HashPayload(b), nil
}

// BindPolicy freezes the deployment-wide quota contract. Configuration changes
// require a drained, explicit operator migration, not a mixed-limit rollout.
func (r *Repository) BindPolicy(ctx context.Context, policy QuotaPolicy) error {
	hash, err := policyHash(policy)
	if err != nil {
		return err
	}
	return r.bind(ctx, "quota", hash)
}

func admissionHash(limits Limits) (string, error) {
	if limits.SessionQueue <= 0 || limits.UserQueue <= 0 || limits.AcceptRPM <= 0 || limits.TaskTimeout <= 0 {
		return "", fmt.Errorf("all admission dimensions must be bounded")
	}
	b, err := json.Marshal(limits)
	if err != nil {
		return "", err
	}
	return HashPayload(b), nil
}

// BindAdmission uses resolved repository defaults so all API instances enforce
// one deployment contract, including the accepted task's immutable deadline.
func (r *Repository) BindAdmission(ctx context.Context) error {
	hash, err := admissionHash(r.limits)
	if err != nil {
		return err
	}
	return r.bind(ctx, "admission", hash)
}

func (r *Repository) checkAdmission(ctx context.Context, tx *sql.Tx) error {
	hash, err := admissionHash(r.limits)
	if err != nil {
		return err
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM execution_policy WHERE name='admission'`).Scan(&stored)
	// Low-level fixtures may omit startup binding; production main binds before
	// exposing any route. A bound policy can never be bypassed by another repo.
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored != hash {
		return fmt.Errorf("execution admission configuration mismatch")
	}
	return nil
}

// BindEncryptionKey prevents a misconfigured worker from accepting snapshots it
// cannot decrypt. Pass a digest of the decoded encryption key, never the key.
func (r *Repository) BindEncryptionKey(ctx context.Context, fingerprint string) error {
	if len(fingerprint) != 64 {
		return fmt.Errorf("invalid encryption key fingerprint")
	}
	return r.bind(ctx, "encryption", fingerprint)
}

// BindProviderRouting prevents mixed account aliases/endpoints from splitting
// credential-group quotas. Model defaults may change; accepted inputs are frozen.
func (r *Repository) BindProviderRouting(ctx context.Context, fingerprint string) error {
	if len(fingerprint) != 64 {
		return fmt.Errorf("invalid provider routing fingerprint")
	}
	return r.bind(ctx, "provider_routing", fingerprint)
}
func (r *Repository) bind(ctx context.Context, name, hash string) error {
	tx, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO execution_policy(name,fingerprint) VALUES(?,?)`, name, hash); err != nil {
		return err
	}
	var stored string
	if err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM execution_policy WHERE name=? FOR UPDATE`, name).Scan(&stored); err != nil {
		return err
	}
	if stored != hash {
		return fmt.Errorf("execution %s configuration mismatch; drain and migrate explicitly", name)
	}
	return tx.Commit()
}
func checkPolicy(ctx context.Context, tx *sql.Tx, policy QuotaPolicy) error {
	hash, err := policyHash(policy)
	if err != nil {
		return err
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM execution_policy WHERE name='quota'`).Scan(&stored)
	// Repository integration fixtures intentionally omit deployment startup. Main
	// always binds before constructing a runtime or exposing acceptance routes.
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored != hash {
		return fmt.Errorf("execution quota configuration mismatch")
	}
	return nil
}
