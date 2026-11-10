package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEC2PoolKeyFile(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("Ab3", 22) // 66 bytes, like the template's secret
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("secret-but-short\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := FromFlags(baseArgs())
	if err != nil || c.EC2PoolKeyFile != "" {
		t.Fatalf("default: %q, %v", c.EC2PoolKeyFile, err)
	}
	if k, err := c.ReadEC2PoolKey(); k != nil || err != nil {
		t.Fatalf("unset: %q, %v; want none and no error", k, err)
	}

	c, err = FromFlags(append(baseArgs(), "--ec2-pool-key-file", good))
	if err != nil {
		t.Fatal(err)
	}
	if k, err := c.ReadEC2PoolKey(); err != nil || string(k) != key {
		t.Fatalf("flag: %q, %v; want the key without its newline", k, err)
	}
	t.Setenv("FR_EC2_POOL_KEY_FILE", good)
	if c, err = FromFlags(baseArgs()); err != nil || c.EC2PoolKeyFile != good {
		t.Fatalf("env: %q, %v", c.EC2PoolKeyFile, err)
	}

	for name, path := range map[string]string{"missing": filepath.Join(dir, "none"), "short": short} {
		c.EC2PoolKeyFile = path
		k, err := c.ReadEC2PoolKey()
		if err == nil || k != nil {
			t.Errorf("%s: %q, %v; want an error", name, k, err)
		} else if strings.Contains(err.Error(), "secret-but-short") {
			t.Errorf("%s: the error holds the key: %v", name, err)
		}
	}
}

func TestEC2PoolKeyFileIsNoFlagDefault(t *testing.T) {
	// The path is not shown as a default in -h; it is read after parsing.
	t.Setenv("FR_EC2_POOL_KEY_FILE", "/etc/firerunner/pool-key")
	fs := newFlagSet(&Config{}, &flagVals{}, &envReader{})
	if f := fs.Lookup("ec2-pool-key-file"); f == nil || f.DefValue != "" {
		t.Fatalf("flag %+v, want an empty default", f)
	}
}
