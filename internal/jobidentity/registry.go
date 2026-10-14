package jobidentity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const leaseTTL = 90 * time.Second

type Resolver interface {
	Resolve(context.Context, Job) (Identity, error)
}
type lease struct {
	identity Identity
	cancel   context.CancelFunc
}

// Registry owns a dedicated directory exclusively. A lock prevents two daemons
// from clearing each other's records. Keep it alive until all VMs have drained.
type Registry struct {
	mu     sync.Mutex
	dir    string
	lock   *os.File
	leases map[string]*lease
	stop   chan struct{}
	done   chan struct{}
	log    *slog.Logger
}

func NewRegistry(dir string, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("jobs directory must be a private directory (0700)")
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("jobs directory already owned: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err == nil {
		for _, path := range files {
			if err = os.Remove(path); err != nil {
				break
			}
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	r := &Registry{dir: dir, lock: f, leases: map[string]*lease{}, stop: make(chan struct{}), done: make(chan struct{}), log: log}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(leaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.refresh()
			}
		}
	}()
	return r, nil
}

func (r *Registry) write(i Identity) error {
	i.ExpiresAt = time.Now().Add(leaseTTL).Unix()
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.dir, ".identity-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(r.dir, i.IP+".json"))
}

// Register runs before the VM can execute. No job rights exist until Start.
func (r *Registry) Register(runner, ip string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() || runner == "" {
		return errors.New("invalid VM identity")
	}
	if _, exists := r.leases[runner]; exists {
		return errors.New("runner already registered")
	}
	for _, l := range r.leases {
		if l.identity.IP == ip {
			return errors.New("address already registered")
		}
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	i := Identity{IP: ip, Runner: runner, Generation: hex.EncodeToString(b[:])}
	if err := r.write(i); err != nil {
		return err
	}
	r.leases[runner] = &lease{identity: i}
	return nil
}

// Release cancels outstanding lookups before removing the file. On error the
// caller MUST quarantine the slot (do not give the address to another VM).
func (r *Registry) Release(runner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.leases[runner]
	if l == nil {
		return nil
	}
	if l.cancel != nil {
		l.cancel()
	}
	delete(r.leases, runner)
	err := os.Remove(filepath.Join(r.dir, l.identity.IP+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Start is nonblocking and idempotent. It never stalls scale-set message polling.
// Responses from a released lease cannot publish into a recycled address.
func (r *Registry) Start(ctx context.Context, j Job, resolver Resolver) {
	r.mu.Lock()
	l := r.leases[j.Runner]
	if l == nil || l.cancel != nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	l.cancel = cancel
	r.mu.Unlock()
	go func() {
		defer cancel()
		var i Identity
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			i, err = resolver.Resolve(ctx, j)
			if err == nil || ctx.Err() != nil {
				break
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.leases[j.Runner] != l || ctx.Err() != nil {
			return
		}
		if err != nil {
			r.log.Warn("cache identity unavailable; writes disabled", "runner", j.Runner, "err", err)
			return
		}
		i.IP, i.Runner, i.Generation = l.identity.IP, j.Runner, l.identity.Generation
		i.ExpiresAt = time.Now().Add(leaseTTL).Unix()
		if err = i.Validate(time.Now()); err == nil {
			err = r.write(i)
		}
		if err != nil {
			r.log.Warn("publish cache identity", "runner", j.Runner, "err", err)
			return
		}
		l.identity = i
	}()
}

func (r *Registry) refresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, l := range r.leases {
		if err := r.write(l.identity); err != nil {
			r.log.Warn("renew cache identity", "runner", name, "err", err)
		}
	}
}

func (r *Registry) Close() {
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, l := range r.leases {
		if l.cancel != nil {
			l.cancel()
		}
		_ = os.Remove(filepath.Join(r.dir, l.identity.IP+".json"))
		delete(r.leases, name)
	}
	_ = r.lock.Close()
}
