package sdk

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain guards against goroutine leaks in the sdk package. Every test
// MUST defer c.Close() on every sdk.Client it creates — Close cancels the
// watchLoop goroutine and wg.Wait()s for it. If a future test forgets the
// defer, goleak flags it here instead of letting a zombie watcher survive
// into production-shaped long-running test suites.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
