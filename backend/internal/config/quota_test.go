package config

import "testing"

func TestSharedQuotaDefaultsPreserveExplicitLimits(t *testing.T) {
	defaults := (AsyncConfig{}).WithQuotaDefaults()
	if defaults.Concurrency != 4 || defaults.UserConcurrency != 2 || defaults.RPM != 60 || defaults.TPM != 200000 {
		t.Fatalf("unexpected shared defaults: %+v", defaults)
	}
	explicit := AsyncConfig{Concurrency: 1, UserConcurrency: 1, RPM: 2, TPM: 5000, Paused: true, Workers: 3}
	if explicit.WithQuotaDefaults() != explicit {
		t.Fatal("budget defaults altered configured limits")
	}
}
