package store

import (
	"testing"
	"time"
)

func TestWatchCacheAppendAndGet(t *testing.T) {
	wc := NewWatchCache(10)
	defer wc.Close()

	// Append 3 events.
	for i := uint64(1); i <= 3; i++ {
		wc.Append(Event{
			Type:  EventPut,
			Entry: &Entry{Key: "k", Revision: i},
		})
	}

	// Get events after rev 0 -> all 3.
	events := wc.WaitForEvents(0, "", 100*time.Millisecond)
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}

	// Get events after rev 2 -> only rev 3.
	events = wc.WaitForEvents(2, "", 100*time.Millisecond)
	if len(events) != 1 || events[0].Entry.Revision != 3 {
		t.Fatalf("expected 1 event at rev 3, got %v", events)
	}
}

func TestWatchCacheRingOverflow(t *testing.T) {
	wc := NewWatchCache(5) // Small buffer
	defer wc.Close()

	// Write 8 events -> first 3 should be lost.
	for i := uint64(1); i <= 8; i++ {
		wc.Append(Event{
			Type:  EventPut,
			Entry: &Entry{Key: "k", Revision: i},
		})
	}

	if wc.Len() != 5 {
		t.Fatalf("expected buffer len 5, got %d", wc.Len())
	}

	// Get all -> should only return events 4-8.
	events := wc.WaitForEvents(0, "", 100*time.Microsecond)
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}

	if events[0].Entry.Revision != 4 {
		t.Fatalf("expected oldest event rev=4, got %d", events[0].Entry.Revision)
	}
}
