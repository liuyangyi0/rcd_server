package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/lowerlogs"
)

func TestLowerLogIngestIsIdempotentAndRejectsSequenceGaps(t *testing.T) {
	runtime := seededRuntime(t)
	logStore, err := lowerlogs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime.lowerLogs = logStore
	now := time.Now().UTC()
	batch := api.LowerLogBatch{SourceID: "source-a", BootID: "boot-a", FirstSequence: 1, Events: []api.LowerLogEvent{
		{SourceID: "source-a", BootID: "boot-a", Sequence: 1, Timestamp: now, Level: "INFO", Component: "serial", PortID: "/dev/ttyWCH0", DeviceID: "11", Message: "poll ok"},
		{SourceID: "source-a", BootID: "boot-a", Sequence: 2, Timestamp: now.Add(time.Millisecond), Level: "ERROR", Component: "serial", PortID: "/dev/ttyWCH0", DeviceID: "13", Message: "timeout"},
	}}
	ack, err := runtime.IngestLowerLogs(context.Background(), batch)
	if err != nil || ack.AcceptedThrough != 2 {
		t.Fatalf("first ack=%#v err=%v", ack, err)
	}
	ack, err = runtime.IngestLowerLogs(context.Background(), batch)
	if err != nil || ack.AcceptedThrough != 2 {
		t.Fatalf("retry ack=%#v err=%v", ack, err)
	}
	page, err := runtime.ListLowerLogs(context.Background(), api.LowerLogRequest{SourceID: "source-a", Level: "ERROR", Query: "TIME", Limit: 10})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Sequence != 2 {
		t.Fatalf("page=%#v err=%v", page, err)
	}

	gap := api.LowerLogBatch{SourceID: "source-a", BootID: "boot-a", FirstSequence: 4, Events: []api.LowerLogEvent{
		{SourceID: "source-a", BootID: "boot-a", Sequence: 4, Timestamp: now, Level: "WARN", Component: "test", Message: "gap"},
	}}
	_, err = runtime.IngestLowerLogs(context.Background(), gap)
	var sequence *api.LowerLogSequenceError
	if !errors.As(err, &sequence) || sequence.Expected != 3 || sequence.Actual != 4 {
		t.Fatalf("gap error=%v", err)
	}
}

func TestLowerLogIngestRejectsUnknownSource(t *testing.T) {
	runtime := seededRuntime(t)
	logStore, err := lowerlogs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtime.lowerLogs = logStore
	_, err = runtime.IngestLowerLogs(context.Background(), api.LowerLogBatch{SourceID: "missing", BootID: "boot", FirstSequence: 1,
		Events: []api.LowerLogEvent{{SourceID: "missing", BootID: "boot", Sequence: 1, Timestamp: time.Now().UTC(), Level: "INFO", Message: "x"}}})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error=%v, want not found", err)
	}
}
