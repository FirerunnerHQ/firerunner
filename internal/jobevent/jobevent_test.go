package jobevent

import (
	"testing"
	"time"
)

type sink struct{ got []Event }

func (s *sink) Record(e Event) { s.got = append(s.got, e) }

func TestRecord(t *testing.T) {
	Record(nil, Event{Kind: Assigned}) // a nil sink records nothing

	s := &sink{}
	before := time.Now()
	Record(s, Event{Kind: Assigned})
	if len(s.got) != 1 || s.got[0].At.Before(before) {
		t.Fatalf("an event with no time is stamped: %+v", s.got)
	}
	at := time.Unix(100, 0)
	Record(s, Event{Kind: Running, At: at})
	if !s.got[1].At.Equal(at) {
		t.Fatalf("an event's own time stands: %v", s.got[1].At)
	}
}

func TestForTenant(t *testing.T) {
	if ForTenant(nil, "acme") != nil {
		t.Fatal("a nil sink stays nil")
	}
	s := &sink{}
	if ForTenant(s, "") != Sink(s) {
		t.Fatal("the install's events go to the sink as they are")
	}
	Record(ForTenant(s, "acme"), Event{Kind: Assigned})
	Record(ForTenant(s, "acme"), Event{Kind: Flagged, Tenant: "other"})
	if s.got[0].Tenant != "acme" || s.got[1].Tenant != "other" {
		t.Fatalf("tenants %q, %q: want acme, and the event's own", s.got[0].Tenant, s.got[1].Tenant)
	}
}
