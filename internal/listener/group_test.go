package listener

import (
	"context"
	"testing"
)

// A runner group given by id is used as it is: no lookup by name, which
// could find another group of the same name. The client is nil, so any
// call on it would panic.
func TestRunnerGroupByID(t *testing.T) {
	id, err := runnerGroupID(context.Background(), nil, Config{RunnerGroup: "firerunner", RunnerGroupID: 42})
	if err != nil || id != 42 {
		t.Fatalf("runnerGroupID = %d, %v; want 42", id, err)
	}
	id, err = runnerGroupID(context.Background(), nil, Config{RunnerGroup: "default"})
	if err != nil || id != 1 {
		t.Fatalf("the default group = %d, %v; want 1", id, err)
	}
}
