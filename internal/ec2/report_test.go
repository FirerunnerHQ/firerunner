package ec2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/jitserver"
)

// report posts a runner report as the instance at ip would.
func (h *harness) report(t *testing.T, ip, query string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, jitserver.ReportPath+"?"+query, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.jit.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("report %s = %d, want 204", query, rec.Code)
	}
}

// TestLaunch_RunnerExitStatus: an instance that reports a non-zero exit fails
// the launch, so the scheduler backs off; a zero exit, or no report at all (a
// hard crash), does not.
func TestLaunch_RunnerExitStatus(t *testing.T) {
	for _, c := range []struct {
		name    string
		reports []string
		fail    bool
	}{
		{"non-zero", []string{"phase=listening", "phase=exited&rc=1"}, true},
		{"never started", []string{"phase=exited&rc=127"}, true},
		{"zero", []string{"phase=listening", "phase=job", "phase=exited&rc=0"}, false},
		{"no report", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil)
			errc := h.launch(context.Background())
			inst := h.instance(t)
			if code, _ := h.fetch(inst.ip); code != http.StatusOK {
				t.Fatalf("fetch = %d", code)
			}
			for _, q := range c.reports {
				h.report(t, inst.ip, q)
			}
			h.f.setState(inst.id, types.InstanceStateNameShuttingDown)
			err := waitErr(t, errc)
			if got := errors.Is(err, ErrRunnerFailed); got != c.fail {
				t.Fatalf("Launch = %v; ErrRunnerFailed %v, want %v", err, got, c.fail)
			}
		})
	}
}

// TestLaunch_RunnerExitIgnoredWhenRemoved: a runner the scheduler removed
// ends however it ends; that is not a failure of the image.
func TestLaunch_RunnerExitIgnoredWhenRemoved(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := h.launch(ctx)
	inst := h.instance(t)
	h.fetch(inst.ip)
	h.report(t, inst.ip, "phase=listening")
	cancel()
	eventually(t, "the terminate request", func() bool { return len(h.f.terminatedIDs()) == 1 })
	h.report(t, inst.ip, "phase=exited&rc=143")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v, want nil for a removed runner", err)
	}
}

// TestLaunch_PooledRunnerExitStatus: the pool path reads the report too.
func TestLaunch_PooledRunnerExitStatus(t *testing.T) {
	h := readyPool(t, 1)
	pooledID := h.f.launched()[0].id
	errc := h.launch(context.Background())
	eventually(t, "the pooled instance to start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	in := h.f.instance(pooledID)
	if code, _ := h.fetch(in.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, in.ip, "phase=exited&rc=2")
	h.f.setState(pooledID, types.InstanceStateNameShuttingDown)
	if err := waitErr(t, errc); !errors.Is(err, ErrRunnerFailed) {
		t.Fatalf("Launch = %v, want ErrRunnerFailed", err)
	}
}
