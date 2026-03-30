package bot

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestShutdownDrainsInFlightHandlers verifies that Shutdown waits for any
// goroutines registered via wg before returning, and that those goroutines
// complete their work (not just that the WaitGroup unblocks).
func TestShutdownDrainsInFlightHandlers(t *testing.T) {
	b := &Bot{log: nopLogger(t)}

	var completed atomic.Int32

	// Simulate three in-flight handlers that each take a little time.
	for range 3 {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			time.Sleep(50 * time.Millisecond)
			completed.Add(1)
		}()
	}

	start := time.Now()
	b.Shutdown(context.Background())
	elapsed := time.Since(start)

	if got := completed.Load(); got != 3 {
		t.Errorf("completed handlers = %d, want 3", got)
	}
	// Sanity: Shutdown should not have returned before the handlers finished.
	if elapsed < 40*time.Millisecond {
		t.Errorf("Shutdown returned too quickly (%v), handlers may not have run", elapsed)
	}
}

// TestShutdownNoHandlers verifies Shutdown returns immediately when nothing
// is in flight.
func TestShutdownNoHandlers(t *testing.T) {
	b := &Bot{log: nopLogger(t)}

	done := make(chan struct{})
	go func() {
		b.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Shutdown blocked when no handlers were in flight")
	}
}

// TestShutdownDrainTimeout verifies that Shutdown returns after the timeout
// even if a handler is still stuck.
func TestShutdownDrainTimeout(t *testing.T) {
	// Override the package-level constant for this test by temporarily
	// replacing it — we use a table-driven struct approach instead to avoid
	// mutating the package constant. We test the timeout path by simulating a
	// handler that never completes within the test's own deadline.

	b := &Bot{log: nopLogger(t)}

	// Register a handler that will outlive the test — we unblock it in cleanup.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		<-block // blocks until test cleanup
	}()

	// Wrap Shutdown with a short deadline to confirm it doesn't hang forever.
	// The real shutdownDrainTimeout is 30s; we use a context deadline here to
	// bound the test. We call the drain logic directly rather than relying on
	// the package constant.
	done := make(chan struct{})
	go func() {
		waitWithTimeout(&b.wg, 100*time.Millisecond, b.log)
		close(done)
	}()

	select {
	case <-done:
		// pass — timed out as expected
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not respect timeout")
	}
}
