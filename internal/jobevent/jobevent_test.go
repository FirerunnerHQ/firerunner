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
