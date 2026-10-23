package listener

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// libClient behaves like scaleset.Client: it holds one mutex across each
// request, so calls on the same client run one at a time.
type libClient struct {
	mu      sync.Mutex
	latency time.Duration
	calls   *atomic.Int32
}

func (c *libClient) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls.Add(1)
	time.Sleep(c.latency)
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "jit-" + s.Name}, nil
}

func poolSource(limit int, latency time.Duration, made *atomic.Int32) *JITSource {
	var calls atomic.Int32
	return &JITSource{
		clients: newClientPool(limit, func() (jitClient, error) {
			made.Add(1)
			return &libClient{latency: latency, calls: &calls}, nil
		}),
		namePrefix: "t",
	}
}

// generateAll runs n Generate calls at once and returns their latencies,
// sorted.
func generateAll(t *testing.T, src *JITSource, n int) []time.Duration {
	t.Helper()
	var wg sync.WaitGroup
	lat := make([]time.Duration, n)
	start := time.Now()
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, jit, err := src.Generate(context.Background(), core.RunnerSpec{})
			if err != nil || jit != "jit-"+name {
				t.Errorf("Generate = %q, %q, %v", name, jit, err)
			}
			lat[i] = time.Since(start)
		}()
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat
}

func TestJITSource_ConcurrentGenerateDoesNotSerialize(t *testing.T) {
	// #49: 20 launches at once must not queue on one client's mutex. With a
	// pool as large as the burst, p95 stays within 1.5x of a single call.
	const latency = 100 * time.Millisecond
	var made atomic.Int32
	single := generateAll(t, poolSource(20, latency, &made), 1)[0]
	lat := generateAll(t, poolSource(20, latency, &made), 20)
	if p95, limit := lat[len(lat)*95/100-1], single*3/2; p95 > limit {
		t.Fatalf("p95 of 20 concurrent Generate = %v, single = %v; want <= %v", p95, single, limit)
	}
}

func TestJITSource_PoolIsBoundedAndReused(t *testing.T) {
	var made atomic.Int32
	src := poolSource(4, 20*time.Millisecond, &made)
	generateAll(t, src, 12)
	if n := made.Load(); n > 4 {
		t.Fatalf("made %d clients, limit is 4", n)
	}
	before := made.Load()
	generateAll(t, src, 4)
	if made.Load() != before {
		t.Fatalf("idle clients were not reused: made %d more", made.Load()-before)
	}
}

func TestClientPool_SeedCountsTowardLimit(t *testing.T) {
	var made atomic.Int32
	var calls atomic.Int32
	p := newClientPool(2, func() (jitClient, error) {
		made.Add(1)
		return &libClient{calls: &calls}, nil
	})
	p.seed(&libClient{calls: &calls})
	a, _ := p.get(context.Background())
	b, _ := p.get(context.Background())
	if made.Load() != 1 {
		t.Fatalf("made %d clients beside the seed, want 1 (limit 2)", made.Load())
	}
	// At the limit, get waits for a client to come back.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("get at the limit = %v, want it to wait until ctx ends", err)
	}
	p.put(a)
	if c, err := p.get(context.Background()); err != nil || c != a {
		t.Fatalf("get after put = %v, %v; want the returned client", c, err)
	}
	p.put(b)
}

func TestClientPool_MakeErrorFreesItsPlace(t *testing.T) {
	fail := true
	var calls atomic.Int32
	p := newClientPool(1, func() (jitClient, error) {
		if fail {
			return nil, errors.New("no credentials")
		}
		return &libClient{calls: &calls}, nil
	})
	if _, err := p.get(context.Background()); err == nil {
		t.Fatal("want the make error")
	}
	fail = false
	if _, err := p.get(context.Background()); err != nil {
		t.Fatalf("a failed make must not use up the pool: %v", err)
	}
}

func TestJitParallelism(t *testing.T) {
	for in, want := range map[int]int{0: 1, 1: 1, 8: 8, 32: 32, 200: maxJITClients} {
		if got := jitParallelism(in); got != want {
			t.Errorf("jitParallelism(%d) = %d, want %d", in, got, want)
		}
	}
}
