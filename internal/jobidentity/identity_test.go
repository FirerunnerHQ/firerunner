package jobidentity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func privateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	return d
}

func identity() Identity {
	return Identity{IP: "172.16.0.2", Runner: "runner", Generation: "generation", Repository: "owner/repo", WriteRef: "refs/pull/1/merge", ReadRefs: []string{"refs/pull/1/merge", "refs/heads/main"}, RunID: 1, JobID: "job", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestIdentityValidation(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Identity)
	}{
		{"no repo", func(i *Identity) { i.Repository = "" }}, {"path traversal", func(i *Identity) { i.Repository = "../repo" }},
		{"expired", func(i *Identity) { i.ExpiresAt = time.Now().Unix() }}, {"no generation", func(i *Identity) { i.Generation = "" }},
		{"no job", func(i *Identity) { i.JobID = "" }}, {"no run", func(i *Identity) { i.RunID = 0 }}, {"no runner", func(i *Identity) { i.Runner = "" }},
		{"bad write ref", func(i *Identity) { i.WriteRef = "main" }}, {"no reads", func(i *Identity) { i.ReadRefs = nil }},
		{"bad read ref", func(i *Identity) { i.ReadRefs = []string{"refs/pull/1/merge", "../../main"} }},
		{"foreign write", func(i *Identity) { i.WriteRef = "refs/heads/main" }},
	}
	if err := identity().Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := identity()
			tt.change(&i)
			if i.Validate(time.Now()) == nil {
				t.Fatal("accepted invalid identity")
			}
		})
	}
	i := identity()
	i.WriteRef = ""
	if err := i.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"refs/heads/", "refs/heads/a..b", "refs/heads/a b", "refs/heads/a\\b", "refs/heads/a\nb", "main"} {
		if ValidRef(r) {
			t.Errorf("accepted %q", r)
		}
	}
	if !i.CanRead("refs/heads/main") || i.CanRead("") || i.CanRead("refs/pull/2/merge") {
		t.Fatal("incorrect readable refs")
	}
	if got := refs("refs/heads/main", "refs/heads/main", "refs/heads/main"); !reflect.DeepEqual(got, []string{"refs/heads/main"}) {
		t.Fatal(got)
	}
}

func TestReaderFailClosed(t *testing.T) {
	dir := privateDir(t)
	r := Reader{Dir: dir}
	i := identity()
	write := func(i Identity) {
		t.Helper()
		b, _ := json.Marshal(i)
		if err := os.WriteFile(filepath.Join(dir, "172.16.0.2.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Lookup("172.16.0.2"); err == nil {
		t.Fatal("missing accepted")
	}
	write(i)
	if got, err := r.Lookup("::ffff:172.16.0.2"); err != nil || got.JobID != i.JobID {
		t.Fatalf("mapped address: %v %v", got, err)
	}
	for _, addr := range []string{"../../evil", "not-an-ip", "::1"} {
		if _, err := r.Lookup(addr); err == nil {
			t.Fatal("invalid address accepted")
		}
	}
	i.ExpiresAt = 1
	write(i)
	if _, err := r.Lookup(i.IP); err == nil {
		t.Fatal("expired accepted")
	}
	i = identity()
	i.IP = "172.16.1.2"
	write(i)
	if _, err := r.Lookup("172.16.0.2"); err == nil {
		t.Fatal("wrong address accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "172.16.0.2.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("172.16.0.2"); err == nil {
		t.Fatal("corrupt accepted")
	}
}

type resolveFunc func(context.Context, Job) (Identity, error)

func (f resolveFunc) Resolve(c context.Context, j Job) (Identity, error) { return f(c, j) }
func awaitIdentity(t *testing.T, r Reader, ip string) Identity {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		i, err := r.Lookup(ip)
		if err == nil {
			return i
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("identity never published")
	return Identity{}
}
func TestRegistryLifecycle(t *testing.T) {
	dir := privateDir(t)
	r, err := NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if second, err := NewRegistry(dir, nil); err == nil {
		second.Close()
		t.Fatal("two owners accepted")
	}
	reader := Reader{Dir: dir}
	if err := r.Register("runner", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Lookup("172.16.0.2"); err == nil {
		t.Fatal("pending VM may write")
	}
	if err := r.Register("other", "172.16.0.2"); err == nil {
		t.Fatal("duplicate address accepted")
	}
	if err := r.Register("runner", "172.16.1.2"); err == nil {
		t.Fatal("duplicate runner accepted")
	}
	if err := r.Register("bad", "garbage"); err == nil {
		t.Fatal("bad address accepted")
	}
	job := Job{Runner: "runner", Repository: "owner/repo", JobID: "job", RunID: 1}
	r.Start(context.Background(), job, resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil }))
	first := awaitIdentity(t, reader, "172.16.0.2")
	r.refresh()
	again := awaitIdentity(t, reader, "172.16.0.2")
	if again.Generation != first.Generation {
		t.Fatal("renewal changed generation")
	}
	if err := r.Release("runner"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Lookup("172.16.0.2"); err == nil {
		t.Fatal("revocation cached")
	}
	if err := r.Register("next", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	job.Runner = "next"
	r.Start(context.Background(), job, resolveFunc(func(context.Context, Job) (Identity, error) { return identity(), nil }))
	next := awaitIdentity(t, reader, "172.16.0.2")
	if next.Generation == first.Generation {
		t.Fatal("reused generation")
	}
}

func TestLateLookupCannotPublishIntoReusedSlot(t *testing.T) {
	r, err := NewRegistry(privateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	started, finish, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if err := r.Register("old", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	r.Start(context.Background(), Job{Runner: "old"}, resolveFunc(func(context.Context, Job) (Identity, error) {
		close(started)
		<-finish
		defer close(returned)
		return identity(), nil
	}))
	<-started
	if err := r.Release("old"); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("next", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	close(finish)
	<-returned
	// Synchronize via publishing the new job; the old result is never authorized.
	r.Start(context.Background(), Job{Runner: "next"}, resolveFunc(func(context.Context, Job) (Identity, error) { i := identity(); i.JobID = "next-job"; return i, nil }))
	got := awaitIdentity(t, Reader{Dir: r.dir}, "172.16.0.2")
	if got.JobID != "next-job" {
		t.Fatal(got)
	}
}

func TestRegistryRestartRemovesStaleIdentities(t *testing.T) {
	dir := privateDir(t)
	b, _ := json.Marshal(identity())
	if err := os.WriteFile(filepath.Join(dir, "172.16.0.2.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{Dir: dir}).Lookup("172.16.0.2"); err == nil {
		t.Fatal("stale accepted")
	}
	r.Close()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(dir, nil); err == nil {
		t.Fatal("public directory accepted")
	}
}

func TestReleaseFailureInvalidatesLease(t *testing.T) {
	r, err := NewRegistry(privateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Register("runner", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, "172.16.0.2.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Release("runner"); err == nil {
		t.Fatal("missing revocation error")
	}
	r.mu.Lock()
	_, present := r.leases["runner"]
	r.mu.Unlock()
	if present {
		t.Fatal("failed release still renews lease")
	}
}

func TestResolverFailureCannotGrantRights(t *testing.T) {
	r, err := NewRegistry(privateDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Register("runner", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})
	r.Start(ctx, Job{Runner: "runner"}, resolveFunc(func(context.Context, Job) (Identity, error) {
		cancel()
		close(called)
		return Identity{}, errors.New("offline")
	}))
	<-called
	if _, err := (Reader{Dir: r.dir}).Lookup("172.16.0.2"); err == nil {
		t.Fatal("failed resolution authorized")
	}
}
