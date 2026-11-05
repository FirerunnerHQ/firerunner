package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/config"
)

func TestRequireLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:3129", "[::1]:3129", "127.1.2.3:80"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:3129", ":3129", "10.0.0.5:3129", "localhost:3129", "[::]:3129", "nonsense"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("%s was accepted", addr)
		}
	}
}

func TestRunGitProxy_RefusesBadFlags(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"no certificate": {nil, "--cert and --key"},
		"not loopback":   {[]string{"--cert", "c", "--key", "k", "--listen", "0.0.0.0:3129"}, "not a loopback"},
		"no such file":   {[]string{"--cert", "/absent", "--key", "/absent"}, "loading the certificate"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := runGitProxy(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadCacheCAs(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Tiers: []config.Tier{{Name: "t", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{Cache: true}}}}
	cfg.Firecracker.WorkDir = dir
	ca, git, err := loadCacheCAs(cfg)
	if err != nil || ca == nil {
		t.Fatalf("loadCacheCAs = %v, %v", ca, err)
	}
	if git != nil {
		t.Fatal("a CA for github.com with no tier asking")
	}
	if _, err := os.Stat(filepath.Join(dir, "git-tls")); err == nil {
		t.Fatal("a directory for a CA nothing needs")
	}

	cfg.Tiers[0].EC2.GitCache = true
	ca, git, err = loadCacheCAs(cfg)
	if err != nil || ca == nil || git == nil {
		t.Fatalf("loadCacheCAs = %v, %v, %v", ca, git, err)
	}
	if bytes.Equal(ca.CertPEM(), git.CertPEM()) {
		t.Fatal("one CA for both")
	}
}
