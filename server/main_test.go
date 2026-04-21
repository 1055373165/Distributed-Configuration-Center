package server

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak.VerifyTestMain to catch goroutine leaks in the server
// package. Every test path here is synchronous HTTP (httptest.NewRecorder),
// so any non-main goroutine lingering after tests finish is a real bug —
// most likely a WatchCache waiter that was never woken up by Close.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
