package lowerlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanceledQueryDoesNotWaitForSourceWriter(t *testing.T) {
	s, err := openWithOptions(t.TempDir(), 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	mu := s.sourceLock("a")
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.QueryContext(ctx, Filter{SourceID: "a", Limit: 10}); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled query remained blocked behind writer")
	}
}

func TestSnapshotSurvivesAppendAndRotation(t *testing.T) {
	s, err := openWithOptions(t.TempDir(), 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := Event{SourceID: "a", BootID: "b", Sequence: 1, Message: strings.Repeat("x", 900)}
	if err = s.Append([]Event{first}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.snapshot("a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, f := range snap {
			f.file.Close()
		}
	}()
	for i := 2; i <= 5; i++ {
		e := first
		e.Sequence = uint64(i)
		if err = s.Append([]Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	var got []Event
	for _, f := range snap {
		err = scanReverse(context.Background(), f, func(b []byte, large bool) {
			var e Event
			if large || json.Unmarshal(b, &e) != nil {
				t.Fatal("invalid snapshot")
			}
			got = append(got, e)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].Sequence != 1 {
		t.Fatalf("snapshot changed: %+v", got)
	}
}

func TestReverseScanBoundsAndCancellation(t *testing.T) {
	s, _ := Open(t.TempDir())
	dir := s.sourceDirectory("a")
	os.MkdirAll(dir, 0750)
	e := Event{SourceID: "a", BootID: "b", Sequence: 1, Message: "中文"}
	data, _ := json.Marshal(e)
	content := string(data) + "\n" + strings.Repeat("x", maxJSONLineBytes*3) + "\n" + string(data)
	if err := os.WriteFile(dir+"/current.jsonl", []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(Filter{SourceID: "a", Limit: 1})
	if err != nil || page.Total != 1 || page.Skipped != 1 || len(page.Items) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.QueryContext(ctx, Filter{SourceID: "a", Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestConcurrentQueriesAndSources(t *testing.T) {
	s, _ := openWithOptions(t.TempDir(), 2048, 2)
	var wg sync.WaitGroup
	for source := 0; source < 3; source++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 1; i <= 60; i++ {
				if err := s.Append([]Event{{SourceID: id, BootID: "b", Sequence: uint64(i)}}); err != nil {
					t.Error(err)
				}
				if _, err := s.Query(Filter{SourceID: id, Limit: 2}); err != nil {
					t.Error(err)
				}
			}
		}(fmt.Sprint(source))
	}
	wg.Wait()
}
