package raft

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain guards against goroutine leaks in the raft package. HashiCorp
// Raft spawns many internal goroutines (leader loop, heartbeat, TCP
// transport accept loop, snapshot workers). Every test MUST shut down its
// Node via t.Cleanup, otherwise this fails.
//
// If a future version of hashicorp/raft leaves a benign background goroutine
// behind, prefer goleak.IgnoreTopFunction("...") over dropping the guard —
// we want to see any regression immediately.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
