package raft

import (
	"errors"
	"paladin-core/store"
	"testing"
	"time"
)

func singleNode(t *testing.T) *Node {
	t.Helper()
	dir := t.TempDir()
	n, err := NewNode(NodeConfig{
		NodeID:    "node1",
		BindAddr:  "127.0.0.1:0",
		DataDir:   dir,
		Bootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Shutdown() })

	// Wait for leader election
	deadline := time.Now().Add(5 * time.Second)
	for !n.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for leader election")
		}
		time.Sleep(50 * time.Microsecond)
	}
	return n
}

func TestRaftPutAndGet(t *testing.T) {
	n := singleNode(t)

	// Put through Raft.
	result, err := n.Apply(Op{Type: "put", Key: "public/prod/db_host", Value: []byte("10.0.0.1")}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Entry.Version != 1 {
		t.Fatalf("expected version 1, got %d", result.Entry.Version)
	}

	// Get directly from local store.
	entry, err := n.Get("public/prod/db_host")
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Value) != "10.0.0.1" {
		t.Fatalf("expected value '10.0.0.1', got '%s'", string(entry.Value))
	}
}

func TestRaftDelete(t *testing.T) {
	n := singleNode(t)

	_, err := n.Apply(Op{Type: "put", Key: "temp", Value: []byte("val")}, 5*time.Second)
	if err != nil {
		t.Fatal("put error:", err)
	}

	entry, err := n.Get("temp")
	if err != nil {
		t.Fatal("get error:", err)
	}
	if string(entry.Value) != "val" {
		t.Fatalf("expected value 'val', got '%s'", string(entry.Value))
	}

	_, err = n.Apply(Op{Type: "delete", Key: "temp"}, 5*time.Second)
	if err != nil {
		t.Fatal("delete error:", err)
	}

	_, err = n.Get("temp")
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestRaftRevisionIncrement(t *testing.T) {
	n := singleNode(t)

	for i := 0; i < 5; i++ {
		n.Apply(Op{Type: "put", Key: "counter", Value: []byte("v")}, 5*time.Second)
	}

	if n.Rev() != 5 {
		t.Fatalf("expected rev 5, got %d", n.Rev())
	}
}

func TestRaftWatchIntegration(t *testing.T) {
	n := singleNode(t)

	// Put through Raft -> should emit watch event.
	n.Apply(Op{Type: "put", Key: "public/prod/key1", Value: []byte("v1")}, 5*time.Second)

	events := n.Store().WatchCache().WaitForEvents(0, "public/prod/", 1*time.Second)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if string(events[0].Entry.Value) != "v1" {
		t.Fatalf("expected value 'v1', got '%s'", string(events[0].Entry.Value))
	}
}

// TestRaftDeleteMissingKey_PreservesSentinel is the regression test for the
// string-matched-error bug in server/raft_server.go. FSM.Apply used to stuff
// err.Error() into OpResult.Error (string), forcing callers to do fragile
// prefix matches like `err.Error() == "apply error: key not found"`. Now the
// sentinel propagates via %w so errors.Is works end-to-end.
func TestRaftDeleteMissingKey_PreservesSentinel(t *testing.T) {
	n := singleNode(t)

	_, err := n.Apply(Op{Type: "delete", Key: "does-not-exist"}, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for deleting missing key, got nil")
	}
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("expected errors.Is(err, store.ErrKeyNotFound); got %v", err)
	}
}

func TestNotLeaderError(t *testing.T) {
	// Create a non-bootstrapped node — it will never become leader.
	dir := t.TempDir()
	n, err := NewNode(NodeConfig{
		NodeID:    "follower",
		BindAddr:  "127.0.0.1:0",
		DataDir:   dir,
		Bootstrap: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Shutdown()

	_, err = n.Apply(Op{Type: "put", Key: "k", Value: []byte("v")}, 1*time.Second)
	if err != ErrNotLeader {
		t.Fatalf("expected ErrNotLeader, got %v", err)
	}
}
