package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
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
