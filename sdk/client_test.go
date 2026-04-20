package sdk

import (
	"net/http/httptest"
	"paladin-core/server"
	"paladin-core/store"
	"strings"
	"testing"
)

func testSDKServer(t *testing.T) (*httptest.Server, *store.WatchableStore) {
	t.Helper()
	bs, _ := store.NewBoltStore(t.TempDir() + "/test.db")
	ws := store.NewWatchableStore(bs)
	srv := server.New(ws)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ts.Close()
		ws.Close()
	})
	return ts, ws
}

func TestSDKFullPullAndGet(t *testing.T) {
	ts, ws := testSDKServer(t)

	// Seed some data.
	ws.Put("public/prod/db_host", []byte("10.0.0.1"))
	ws.Put("public/prod/db_port", []byte("3306"))

	addr := strings.TrimPrefix(ts.URL, "http://")
	c, err := New(Config{
		Addrs:     []string{addr},
		Tenant:    "public",
		Namespace: "prod",
		CacheDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	v, ok := c.Get("public/prod/db_host")
	if !ok || string(v) != "10.0.0.1" {
		t.Fatalf("expected db_host=10.0.0.1, got ok=%v value=%q", ok, string(v))
	}

	v, ok = c.Get("public/prod/db_port")
	if !ok || string(v) != "3306" {
		t.Fatalf("expected db_port=3306, got ok=%v value=%q", ok, string(v))
	}
}
