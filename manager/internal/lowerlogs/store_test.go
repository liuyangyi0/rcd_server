package lowerlogs

import (
	"testing"
	"time"
)

func TestAppendQueryFiltersAndDeduplicatesRetry(t *testing.T) {
	store, err := openWithOptions(t.TempDir(), 4096, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []Event{
		{SourceID: "lower/a", BootID: "boot", Sequence: 1, Timestamp: now, ReceivedAt: now, Level: "INFO", Component: "serial", PortID: "/dev/ttyWCH0", DeviceID: "11", Message: "poll ok"},
		{SourceID: "lower/a", BootID: "boot", Sequence: 2, Timestamp: now.Add(time.Millisecond), ReceivedAt: now, Level: "ERROR", Component: "serial", PortID: "/dev/ttyWCH0", DeviceID: "13", Message: "timeout"},
	}
	if err := store.Append(events); err != nil {
		t.Fatal(err)
	}
	// A crash between durable append and cursor commit may replay one event.
	if err := store.Append(events[1:]); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Filter{SourceID: "lower/a", Level: "ERROR", Query: "TIME", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Sequence != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestRotationRemainsBounded(t *testing.T) {
	store, err := openWithOptions(t.TempDir(), 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for sequence := uint64(1); sequence <= 20; sequence++ {
		event := Event{SourceID: "lower-a", BootID: "boot", Sequence: sequence, Timestamp: now, ReceivedAt: now, Level: "INFO", Component: "test", Message: string(make([]byte, 300))}
		if err := store.Append([]Event{event}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Filter{SourceID: "lower-a", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == 0 || page.Total >= 20 {
		t.Fatalf("rotation did not bound retained events: %d", page.Total)
	}
}
