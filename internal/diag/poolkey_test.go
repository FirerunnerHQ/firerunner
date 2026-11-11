package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
)

func TestEC2PoolKeyCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	if err := os.WriteFile(good, []byte(strings.Repeat("s", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	pooled := []config.Tier{{Name: "p", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{Pool: 1}}}
	cold := []config.Tier{{Name: "c", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}}
	fc := []config.Tier{{Name: "f"}}
	if c := ec2PoolKeyCheck(&config.Config{Tiers: fc}); c != nil {
		t.Fatalf("no EC2 tier and no key, yet a check: %+v", c)
	}
	for _, c := range []struct {
		name, file, level, detail string
		tiers                     []config.Tier
	}{
		{"pool, no key", "", levelWarn, "the warm pool is rebuilt", pooled},
		// The reaper trusts only runner tags signed with the key.
		{"no pool, no key", "", levelWarn, "deregisters no runner", cold},
		{"pool, key", good, levelPass, good, pooled},
		{"key unreadable", filepath.Join(dir, "none"), levelFail, "does not exist", fc},
	} {
		got := ec2PoolKeyCheck(&config.Config{Tiers: c.tiers, EC2PoolKeyFile: c.file})
		if got == nil || got.Level != c.level || !strings.Contains(got.Detail, c.detail) {
			t.Errorf("%s: %+v, want %s with %q", c.name, got, c.level, c.detail)
		} else if strings.Contains(got.Detail, "ssss") {
			t.Errorf("%s: the check prints the key: %s", c.name, got.Detail)
		}
	}
}
