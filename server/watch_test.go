package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWatchReturnsEventsImmediately(t *testing.T) {
	srv, cleanup := testServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/public/prod/db_host", strings.NewReader("10.0.0.1"))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	req = httptest.NewRequest(http.MethodGet, "/api/v1/watch/public/prod/?revision=0%timeout=1", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp WatchResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(resp.Events))
	}
	if resp.Events[0].Type != "PUT" {
		t.Fatalf("expected ")
	}
}
