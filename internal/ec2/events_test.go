package ec2

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

type eventLog struct {
	mu  sync.Mutex
	got []jobevent.Event
}

func (l *eventLog) Record(e jobevent.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, e)
}

// A job's instance reports what it is, each stage at its own time, and why it
// went, so usage and billing can be built from these alone.
func TestLaunch_RecordsEvents(t *testing.T) {
	events := &eventLog{}
	h := newHarness(t, func(o *Options) { o.Events = events })
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, inst.ip, "phase=listening")
	h.report(t, inst.ip, "phase=job")
	h.report(t, inst.ip, "phase=exited&rc=0")
	h.f.setStateFor(inst.id, types.InstanceStateNameTerminated, "Client.UserInitiatedShutdown")
	waitErr(t, errc)

	events.mu.Lock()
	defer events.mu.Unlock()
	var kinds []jobevent.Kind
	for _, e := range events.got {
		kinds = append(kinds, e.Kind)
		if e.Tier != "cloud-4c" || e.Runner != "runner-1" || e.Instance != inst.id || e.InstanceType != "c8a.xlarge" ||
			e.Market != "on-demand" || e.Subnet != "subnet-a" || e.At.IsZero() {
			t.Errorf("event lost the instance: %+v", e)
		}
	}
	want := []jobevent.Kind{jobevent.Launched, jobevent.InstanceGone, jobevent.Served, jobevent.Listening, jobevent.Running, jobevent.Exited}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if events.got[0].Path != PathCold {
		t.Errorf("path = %q", events.got[0].Path)
	}
	if events.got[1].Reason != "Client.UserInitiatedShutdown" {
		t.Errorf("reason = %q", events.got[1].Reason)
	}
}
