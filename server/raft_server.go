package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	praft "paladin-core/raft"
	"time"
)

// RaftServer extends Server with Raft awaareness.
type RaftServer struct {
	*Server
	node *praft.Node
}

// NewRaftServer creates a Raft-aware HTTP server.
func NewRaftServer(node *praft.Node) *RaftServer {
	// Build the base server (without /api/v1/config/) using the node's WatchableStore.
	base := newBase(node.Store())

	rs := &RaftServer{Server: base, node: node}

	// Install the Raft-aware config handler.
	rs.mux.HandleFunc("/api/v1/config/", rs.handleRaftConfig)

	return rs
}

// handleRaftConfig routes reads locally and writes through Raft (or forwards to Leader).
func (rs *RaftServer) handleRaftConfig(w http.ResponseWriter, r *http.Request) {
	tenant, namespace, name, err := configKey(r.URL.Path)
	if err != nil || tenant == "" {
		httpError(w, http.StatusBadRequest, "invalid path: %s", r.URL.Path)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Reads go directly to local store (stale reads OK by default).
		// For consistent reads, add ?consistent=true (woould call raft.VerifyLeader).
		if name == "" {
			rs.handleList(w, r, tenant, namespace)
		} else {
			rs.handleGet(w, r, tenant, namespace, name)
		}

	case http.MethodPut:
		if name == "" {
			httpError(w, http.StatusBadRequest, "name is required for PUT")
			return
		}
		rs.handleRaftPut(w, r, tenant, namespace, name)

	case http.MethodDelete:
		if name == "" {
			httpError(w, http.StatusBadRequest, "name is required for DELETE")
			return
		}
		rs.handleRaftDelete(w, r, tenant, namespace, name)

	default:
		httpError(w, http.StatusMethodNotAllowed, "method %s not allowed", r.Method)
	}
}

func (rs *RaftServer) handleRaftPut(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body: %v", err)
		return
	}

	key := storeKey(tenant, namespace, name)

	op := praft.Op{Type: "put", Key: key, Value: body}
	result, err := rs.node.Apply(op, 5*time.Second)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "raft apply: %v", err)
		return
	}

	status := http.StatusOK
	if result.PrevEntry == nil {
		status = http.StatusCreated
	}

	w.Header().Set("X-Paladin-Revision", fmt.Sprintf("%d", result.Entry.Revision))
	writeJSON(w, status, &ConfigResponse{
		Revision: result.Entry.Revision,
		Configs:  []*ConfigItem{entryToConfig(result.Entry)},
	})
	log.Printf("[Raft PUT] %s rev=%d", key, result.Entry.Revision)
}

func (rs *RaftServer) handleRaftDelete(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	key := storeKey(tenant, namespace, name)

	op := praft.Op{Type: "delete", Key: key}
	result, err := rs.node.Apply(op, 5*time.Second)
	if err != nil {
		if err.Error() == "apply error: key not found" {
			httpError(w, http.StatusNotFound, "key not found: %s", key)
			return
		}
		httpError(w, http.StatusInternalServerError, "raft apply: %v", err)
		return
	}

	writeJSON(w, http.StatusOK, &ConfigResponse{
		Revision: rs.node.Rev(),
		Configs:  []*ConfigItem{entryToConfig(result.Entry)},
	})
	log.Printf("[RAFT DELETE] %s", key)
}
