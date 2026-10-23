package lowerlogs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPartialAppendRetryRepairsTailWithoutLosingAcknowledgedEvents(t *testing.T) {
	for _, preceding := range []bool{false, true} {
		t.Run(fmt.Sprint(preceding), func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			base := Event{SourceID: "a", BootID: "boot", Sequence: 1, Timestamp: now, Message: "already acknowledged"}
			if preceding {
				if err := s.Append([]Event{base}); err != nil {
					t.Fatal(err)
				}
			}
			events := []Event{{SourceID: "a", BootID: "boot", Sequence: 2, Timestamp: now, Message: "retry first"}, {SourceID: "a", BootID: "boot", Sequence: 3, Timestamp: now, Message: "retry second"}}
			dir := s.sourceDirectory("a")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(dir, "current.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(events[0])
			if _, err := f.Write(raw[:len(raw)/2]); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			if err := s.Append(events); err != nil {
				t.Fatal(err)
			}
			page, err := s.Query(Filter{SourceID: "a", Limit: 20})
			want := 2
			if preceding {
				want = 3
			}
			if err != nil || page.Total != want || page.Skipped != 0 {
				t.Fatalf("page=%+v error=%v", page, err)
			}
		})
	}
}

func TestCommandAttemptsFilteredBeforePagingAndSurviveReopen(t *testing.T) {
	root := t.TempDir()
	storage, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, sent := range []int{1, 3, 2, 3} {
		event := Event{SourceID: "a", BootID: "boot", Sequence: uint64(i + 1), Timestamp: now, Component: "rcd_server::command_audit",
			Message: fmt.Sprintf(`{"schema_version":1,"sent_count":%d,"attempt_count":%d,"max_attempts":3,"result":"received"}`, sent, sent)}
		if err := storage.Append([]Event{event, event}); err != nil {
			t.Fatal(err)
		}
	}
	storage, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	page, err := storage.Query(Filter{SourceID: "a", CommandAttempts: "three", Limit: 1, Offset: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Sequence != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = storage.Query(Filter{SourceID: "a", CommandAttempts: "retried", Limit: 10})
	if err != nil || page.Total != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = storage.Query(Filter{SourceID: "b", CommandAttempts: "all", Limit: 10})
	if err != nil || page.Total != 0 {
		t.Fatalf("source isolation: %+v %v", page, err)
	}
	for _, message := range []string{`not json`, `{}`, `{"schema_version":1,"sent_count":3,"attempt_count":1,"max_attempts":3,"result":"received"}`} {
		if matchesCommandAttempts(Event{Component: "rcd_server::command_audit", Message: message}, "all") {
			t.Fatalf("accepted invalid summary: %s", message)
		}
	}
}

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
