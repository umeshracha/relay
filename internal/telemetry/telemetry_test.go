package telemetry

import (
	"testing"
	"time"
)

func TestFullQueueDropsWithoutWaiting(t *testing.T) {
	recorder := &Recorder{queue: make(chan Event, 1)}
	recorder.Record(Event{RequestID: "first"})
	done := make(chan struct{})
	go func() {
		recorder.Record(Event{RequestID: "dropped"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request recording waited for a full queue")
	}
	if recorder.Dropped() != 1 || (<-recorder.queue).RequestID != "first" {
		t.Fatal("queue failed to preserve its first event and count the drop")
	}
}
