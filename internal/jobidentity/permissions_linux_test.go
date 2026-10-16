//go:build linux

package jobidentity

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Run as root in CI and on the KVM host. The subprocess really drops credentials;
// this catches directory traversal/group mistakes that chmod-only tests miss.
func TestReaderSeparateUID(t *testing.T) {
	if dir := os.Getenv("FR_READER_UID_TEST"); dir != "" {
		owner := 0
		r := Reader{Dir: filepath.Join(dir, "jobs"), OwnerUID: &owner}
		if err := r.Check(); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Lookup("172.16.0.2"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.Dir, "forged.json"), []byte("{}"), 0600); err == nil {
			t.Fatal("reader created record")
		}
		if err := os.WriteFile(filepath.Join(r.Dir, "172.16.0.2.json"), []byte("{}"), 0600); err == nil {
			t.Fatal("reader modified record")
		}
		if err := os.Remove(filepath.Join(r.Dir, "172.16.0.2.json")); err == nil {
			t.Fatal("reader revoked record")
		}
		if _, err := os.ReadFile(filepath.Join(dir, "app-key")); err == nil {
			t.Fatal("reader accessed daemon secret")
		}
		return
	}
	if os.Getuid() != 0 {
		t.Skip("requires root to test a separate UID")
	}
	dir, err := os.MkdirTemp("/tmp", "fr-reader-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(dir, "jobs")
	if err := os.Mkdir(jobs, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(jobs, 0, 65534); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(jobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.write(identity()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app-key"), []byte("fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "reader.test")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^TestReaderSeparateUID$", "-test.v")
	cmd.Env = append(os.Environ(), "FR_READER_UID_TEST="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("separate UID: %v\n%s", err, out)
	}
	t.Log(string(out))
	// Even with readable permissions, a record owned by the reader is untrusted.
	record := filepath.Join(jobs, "172.16.0.2.json")
	if err := os.Chown(record, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	got, err := (Reader{Dir: jobs}).Lookup("172.16.0.2")
	if err == nil {
		raw, _ := json.Marshal(got)
		t.Fatalf("foreign owner trusted: %s", raw)
	}
}
