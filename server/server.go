// Package server provides the HTTP API layer for PaladinCore.
//
// Day2: We expose the KV store via HTTP, add multi-tenant namespace isolation,
// and introduce JWT authentication.
//
// Key design: configuration keys are structured as "tenant/namespace/name"
// (inspired by K8S resource paths). This gives us hierarchical organization
// without needing a relational database.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"paladin-core/store"
	"strings"
)

// Server is the HTTP API server for PaladinCore.
type Server struct {
	store store.Store
	wc    *store.WatchCache
	mux   *http.ServeMux
}

// New creates a new Server backed by the given Store.
// If the store is a WatchableStore, the watch endpoint is enabled.
func New(s store.Store) *Server {
	srv := newBase(s)
	srv.mux.HandleFunc("/api/v1/config/", srv.handleConfig)
	return srv
}

// newBase wires every route except /api/v1/config/, so wrappers like
// RaftServer can install their own config handler without colliding.
// Net/http's ServeMux panics on duplicate patterns since Go 1.22.
func newBase(s store.Store) *Server {
	srv := &Server{store: s, mux: http.NewServeMux()}
	if ws, ok := s.(*store.WatchableStore); ok {
		srv.wc = ws.WatchCache()
	}
	srv.mux.HandleFunc("/api/v1/rev", srv.handleRev)
	if srv.wc != nil {
		srv.registerWatchRoutes()
	}
	srv.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return srv
}

// ServeHTTP implements http.Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// configKey builds the internal store key from URL path segments.
// Path format: /api/v1/config/{tenant}/{namespace}/{name}
// Store key: {tenant}/{namespace}/{name}
func configKey(path string) (tenant, namespace, name string, err error) {
	// Strip the prefix "/api/v1/config/"
	trimmed := strings.TrimPrefix(path, "/api/v1/config/")
	trimmed = strings.TrimSuffix(trimmed, "/")

	parts := strings.SplitN(trimmed, "/", 3)
	switch len(parts) {
	case 1:
		return parts[0], "", "", nil // tenant only
	case 2:
		return parts[0], parts[1], "", nil // tenant + namespace (for listening)
	case 3:
		return parts[0], parts[1], parts[2], nil // full key
	default:
		return "", "", "", fmt.Errorf("invalid path: %s", path)
	}
}

func storeKey(tenant, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", tenant, namespace, name)
}

func listPrefix(tenant, namespace string) string {
	if namespace == "" {
		return tenant + "/"
	}
	return fmt.Sprintf("%s/%s/", tenant, namespace)
}

// handleConfig dispatches to the appropriate handler based on HTTP method.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	tenant, namespace, name, err := configKey(r.URL.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %s", r.URL.Path)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if name == "" {
			// List by prefix
			s.handleList(w, r, tenant, namespace)
		} else {
			s.handleGet(w, r, tenant, namespace, name)
		}
	case http.MethodPut:
		if name == "" {
			httpError(w, http.StatusBadRequest, "name is required for PUT")
			return
		}
		s.handlePut(w, r, tenant, namespace, name)
	case http.MethodDelete:
		if name == "" {
			httpError(w, http.StatusBadRequest, "name is required for DELETE")
			return
		}
		s.handleDelete(w, r, tenant, namespace, name)
	default:
		httpError(w, http.StatusMethodNotAllowed, "method %s not allowed", r.Method)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	key := storeKey(tenant, namespace, name)
	entry, err := s.store.Get(key)
	if err != nil {
		if err == store.ErrKeyNotFound {
			httpError(w, http.StatusNotFound, "key not found: %s", key)
			return
		}
		httpError(w, http.StatusInternalServerError, "failed to get key: %v", err)
		return
	}

	writeJSON(w, http.StatusOK, &ConfigResponse{
		Revision: s.store.Rev(),
		Configs:  []*ConfigItem{entryToConfig(entry)},
	})
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		httpError(w, http.StatusBadRequest, "failed to read request body: %v", err)
		return
	}

	key := storeKey(tenant, namespace, name)
	result, err := s.store.Put(key, body)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "put: %v", err)
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

	log.Printf("[PUT] %s rev=%d ver=%d", key, result.Entry.Revision, result.Entry.Version)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, tenant, namespace, name string) {
	key := storeKey(tenant, namespace, name)

	deleted, err := s.store.Delete(key)
	if err != nil {
		if err == store.ErrKeyNotFound {
			httpError(w, http.StatusNotFound, "key not found: %s", key)
			return
		}
		httpError(w, http.StatusInternalServerError, "failed to delete key: %v", err)
		return
	}

	writeJSON(w, http.StatusOK, &ConfigResponse{
		Revision: s.store.Rev(),
		Configs:  []*ConfigItem{entryToConfig(deleted)},
	})

	log.Printf("[DELETE] %s rev=%d", key, s.store.Rev())
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request, tenant, namespace string) {
	prefix := listPrefix(tenant, namespace)
	entries, err := s.store.List(prefix)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list: %v", err)
		return
	}

	configs := make([]*ConfigItem, len(entries))
	for i, e := range entries {
		configs[i] = entryToConfig(e)
	}

	writeJSON(w, http.StatusOK, &ConfigResponse{
		Revision: s.store.Rev(),
		Count:    len(configs),
		Configs:  configs,
	})
}

func (s *Server) handleRev(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]uint64{"revision": s.store.Rev()})
}

// --- Data Types ---

// ConfigItem is the JSON representation of a configuration entry.
type ConfigItem struct {
	Key            string `json:"key"`
	Value          string `json:"value"`
	Revision       uint64 `json:"revision"`
	CreateRevision uint64 `json:"create_revision"`
	ModRevision    uint64 `json:"mod_revision"`
	Version        uint64 `json:"version"`
}

// ConfigResponse wraps a list of configs with metadata.
type ConfigResponse struct {
	Revision uint64        `json:"revision"`
	Count    int           `json:"count,omitempty"`
	Configs  []*ConfigItem `json:"configs,"`
}

func entryToConfig(e *store.Entry) *ConfigItem {
	return &ConfigItem{
		Key:            e.Key,
		Value:          string(e.Value),
		Revision:       e.Revision,
		CreateRevision: e.CreateRevision,
		ModRevision:    e.ModRevision,
		Version:        e.Version,
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
