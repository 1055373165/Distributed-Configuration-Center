package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"paladin-core/internal/metrics"
	praft "paladin-core/raft"
	"paladin-core/store"
)

// RaftServer extends Server with Raft awaareness.
type RaftServer struct {
	*Server
	node *praft.Node
}

// NewRaftServer creates a Raft-aware HTTP server.
func NewRaftServer(node *praft.Node) *RaftServer {
	// Build the base server WITHOUT /api/v1/config/ — newBase skips it so
	// we can install the Raft-aware handler here without a duplicate-pattern
	// panic from net/http.ServeMux. Do NOT switch this to New(...).
	base := newBase(node.Store())

	rs := &RaftServer{Server: base, node: node}

	// Install the Raft-aware config handler.
	rs.handleFunc("/api/v1/config/", rs.handleRaftConfig)
	// Watch still works the same (reads from WatchCache).
	// Admin endpoints.
	rs.handleFunc("/admin/join", rs.handleJoin)
	rs.handleFunc("/admin/leave", rs.handleLeave)
	rs.handleFunc("/admin/stats", rs.handleStats)
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

	// If not leader, forward to leader.
	if !rs.node.IsLeader() {
		metrics.RaftApplyTotal.WithLabelValues("put", "not_leader").Inc()
		rs.forwardToLeader(w, r, body)
		return
	}

	op := praft.Op{Type: "put", Key: key, Value: body}
	start := time.Now()
	result, err := rs.node.Apply(op, 5*time.Second)
	metrics.RaftApplyDuration.WithLabelValues("put").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RaftApplyTotal.WithLabelValues("put", "error").Inc()
		httpError(w, http.StatusInternalServerError, "raft apply: %v", err)
		return
	}
	metrics.RaftApplyTotal.WithLabelValues("put", "ok").Inc()
	// store_revision gauge is updated inside FSM.Apply so followers track it too.

	status := http.StatusOK
	if result.PrevEntry == nil {
		status = http.StatusCreated
	}

	w.Header().Set("X-Paladin-Revision", fmt.Sprintf("%d", result.Entry.Revision))
	writeJSON(w, status, &ConfigResponse{
		Revision: result.Entry.Revision,
		Configs:  []*ConfigItem{entryToConfig(result.Entry)},
	})
	rs.log.Info("raft put applied", "key", key, "rev", result.Entry.Revision)
}

func (rs *RaftServer) handleRaftDelete(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	key := storeKey(tenant, namespace, name)

	// If not leader, forward to leader.
	if !rs.node.IsLeader() {
		metrics.RaftApplyTotal.WithLabelValues("delete", "not_leader").Inc()
		rs.forwardToLeader(w, r, nil)
		return
	}

	op := praft.Op{Type: "delete", Key: key}
	start := time.Now()
	result, err := rs.node.Apply(op, 5*time.Second)
	metrics.RaftApplyDuration.WithLabelValues("delete").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RaftApplyTotal.WithLabelValues("delete", "error").Inc()
		// errors.Is walks the %w chain, so a renamed wrapper message upstream
		// can't silently turn a 404 into a 500.
		if errors.Is(err, store.ErrKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found: %s", key)
			return
		}
		httpError(w, http.StatusInternalServerError, "raft apply: %v", err)
		return
	}
	metrics.RaftApplyTotal.WithLabelValues("delete", "ok").Inc()
	// store_revision gauge is updated inside FSM.Apply so followers track it too.

	writeJSON(w, http.StatusOK, &ConfigResponse{
		Revision: rs.node.Rev(),
		Configs:  []*ConfigItem{entryToConfig(result.Entry)},
	})
	rs.log.Info("raft delete applied", "key", key)
}

// forwardToLeader proxies the request to the current Raft leader.
//
// This is the core of "ForwardRPC" - the client connects to any node,
// and if that node is a Follower, the request is transparently forwarded
// to the Leader. The client never needs to know cluster topology.
func (rs *RaftServer) forwardToLeader(w http.ResponseWriter, r *http.Request, body []byte) {
	// Use the leader's HTTP advertise address (NOT LeaderAddr, which is the
	// Raft transport port that speaks a binary protocol).
	leaderHTTP := rs.node.LeaderHTTPAddr()
	if leaderHTTP == "" {
		// Either no leader yet, or the leader hasn't been registered in the
		// peer-HTTP map. The latter happens briefly right after bootstrap or
		// failover; client should retry.
		httpError(w, http.StatusServiceUnavailable,
			"leader http addr unknown (leader_raft=%q), try again later",
			rs.node.LeaderAddr())
		return
	}

	// Build forwarded request.
	url := fmt.Sprintf("http://%s%s", leaderHTTP, r.URL.String())
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	fwdReq, err := http.NewRequestWithContext(r.Context(), r.Method, url, bodyReader)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "build forward request: %v", err)
		return
	}

	// Forward headers.
	for k, vs := range r.Header {
		for _, v := range vs {
			fwdReq.Header.Add(k, v)
		}
	}
	fwdReq.Header.Set("X-Forwarded-By", rs.node.Stats()["id"])

	resp, err := http.DefaultClient.Do(fwdReq)
	if err != nil {
		httpError(w, http.StatusBadRequest, "forward to leader %s: %v", leaderHTTP, err)
		return
	}
	defer resp.Body.Close()

	// Copy leader response back to client.
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	rs.log.Info("request forwarded to leader",
		"method", r.Method,
		"path", r.URL.Path,
		"leader", leaderHTTP,
		"status", resp.StatusCode,
	)
}

// --- Admin Endpoints (Day 5/7) ---
func (rs *RaftServer) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	nodeID := r.URL.Query().Get("id")
	addr := r.URL.Query().Get("addr")
	httpAddr := r.URL.Query().Get("http")
	if nodeID == "" || addr == "" || httpAddr == "" {
		httpError(w, http.StatusBadRequest, "id, addr, http required")
		return
	}
	if err := rs.node.Join(nodeID, addr); err != nil {
		httpError(w, http.StatusInternalServerError, "join: %v", err)
		return
	}
	// Replicate the joiner's HTTP advertise address so every node can
	// resolve where to forward writes when the leader changes.
	if err := rs.node.RegisterPeerHTTP(nodeID, httpAddr); err != nil {
		httpError(w, http.StatusInternalServerError, "register http addr: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "joined": nodeID})
	rs.log.Info("node joined cluster", "node", nodeID, "raft_addr", addr, "http_addr", httpAddr)
}

func (rs *RaftServer) handleLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}

	nodeID := r.URL.Query().Get("id")
	if nodeID == "" {
		httpError(w, http.StatusBadRequest, "id required")
		return
	}
	if err := rs.node.Leave(nodeID); err != nil {
		httpError(w, http.StatusInternalServerError, "leave: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "removed": nodeID})
	rs.log.Info("node left cluster", "node", nodeID)
}

func (rs *RaftServer) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := rs.node.Stats()
	stats["store_revision"] = fmt.Sprintf("%d", rs.node.Rev())
	stats["is_leader"] = fmt.Sprintf("%v", rs.node.IsLeader())
	writeJSON(w, http.StatusOK, stats)
}
