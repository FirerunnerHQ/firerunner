package listener

import (
	"context"
	"testing"
)

// CloseSession closes the session and deletes nothing, even a scale set this
// process created without KeepOnClose: its tenant's next shard may serve it
// already. The nil client would panic on a deletion.
func TestCloseSessionKeepsTheScaleSet(t *testing.T) {
	sess := &fakeSession{}
	s := &ScaleSet{log: testLogger(), session: sess, scaleSetID: 4, created: true}
	s.CloseSession(context.Background())
	if !sess.closed.Load() {
		t.Fatal("session left open")
	}
}
