package server

import (
	praft "github.com/smy/paladin-core/raft"
)

// RaftServer extends Server with Raft awaareness.
type RaftServer struct {
	*Server
	node *praft.Raft
}

// NewRaftServer creates a Raft-aware HTTP server.
func NewRaftServer(node *praft.Node) *RaftServer {
	// Build the base server with the node's WatchableStore.
	base := New(node.Store())

}
