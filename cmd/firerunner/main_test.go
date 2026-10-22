package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/provisioner"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"512B", 512, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"50GB", 50_000_000_000, false},
		{"2GiB", 2 << 30, false},
		{"1.5GB", 1_500_000_000, false},
		{" 10 mb ", 10_000_000, false},
		{"-5GB", 0, true},
		{"abc", 0, true},
		{"12xz", 0, true},
	}
	for _, c := range cases {
		got, err := parseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCacheServerRejectsUnsafeScopeConfig(t *testing.T) {
	for _, args := range [][]string{
		{"--jobs-dir", "relative"},
		{"--repository", ""},
		{"--repository", "owner/repo", "--default-ref", "main"},
	} {
		args = append([]string{"--dir", t.TempDir()}, args...)
		if err := cacheServe(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCacheServerChecksIdentityDirectoryAtStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--jobs-dir", dir, "--jobs-owner-uid", strconv.Itoa(os.Getuid() + 1)},
		{"--jobs-dir", dir + "/missing"},
	} {
		args = append([]string{"--dir", t.TempDir()}, args...)
		if err := cacheServe(args); err == nil || !strings.Contains(err.Error(), "job identity directory") {
			t.Fatalf("startup check: %v", err)
		}
	}
}

// stubProvisioner stands in for the host's Firecracker provisioner.
type stubProvisioner struct{ name string }

func (s stubProvisioner) Launch(context.Context, string, string, core.RunnerSpec, func()) error {
	return nil
}
func (s stubProvisioner) Name() string { return s.name }

func TestTierProvisioner(t *testing.T) {
	fc := stubProvisioner{name: "firecracker"}
	ec2 := config.Tier{Name: "cloud", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}

	// Every Firecracker tier, implicit or explicit, shares the one host
	// provisioner and its slot budget.
	for _, tier := range []config.Tier{{Name: "a"}, {Name: "b", Provisioner: config.ProvisionerFirecracker}} {
		p, err := tierProvisioner(tier, fc, nil)
		if err != nil || p != fc {
			t.Fatalf("tier %q: got (%v, %v), want the shared firecracker provisioner", tier.Name, p, err)
		}
	}
	// A Firecracker tier on a host without the Firecracker provisioner is a
	// wiring bug and must fail before any scale set is registered.
	if _, err := tierProvisioner(config.Tier{Name: "a"}, nil, nil); err == nil {
		t.Fatal("expected an error for a firecracker tier without a firecracker provisioner")
	}
	// Each ec2 tier gets its own provisioner, built for that tier.
	var built []string
	newEC2 := func(t config.Tier) provisioner.Provisioner {
		built = append(built, t.Name)
		return stubProvisioner{name: "ec2:" + t.Name}
	}
	p, err := tierProvisioner(ec2, fc, newEC2)
	if err != nil || p.Name() != "ec2:cloud" || len(built) != 1 {
		t.Fatalf("ec2 tier: got (%v, %v), built %v", p, err, built)
	}
	if _, err := tierProvisioner(ec2, fc, nil); err == nil {
		t.Fatal("expected an error for an ec2 tier without the ec2 client")
	}
	if _, err := tierProvisioner(config.Tier{Name: "x", Provisioner: "lambda"}, fc, newEC2); err == nil {
		t.Fatal("expected an error for an unknown provisioner")
	}
}
