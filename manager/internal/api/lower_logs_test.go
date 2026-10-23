package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type lowerLogFakeService struct {
	*fakeService
	batch   LowerLogBatch
	err     error
	request LowerLogRequest
}

func (f *lowerLogFakeService) IngestLowerLogs(_ context.Context, batch LowerLogBatch) (LowerLogAck, error) {
	f.batch = batch
	if f.err != nil {
		return LowerLogAck{}, f.err
	}
	return LowerLogAck{SourceID: batch.SourceID, BootID: batch.BootID, AcceptedThrough: batch.Events[len(batch.Events)-1].Sequence, ReceivedAt: time.Now().UTC()}, nil
}

func (f *lowerLogFakeService) ListLowerLogs(_ context.Context, request LowerLogRequest) (LowerLogPage, error) {
	f.request = request
	return LowerLogPage{Limit: request.Limit, Offset: request.Offset, PushState: "stored"}, nil
}

func TestLowerLogCommandAttemptFilters(t *testing.T) {
	service := &lowerLogFakeService{fakeService: &fakeService{}}
	handler := New(service, Options{})
	for _, value := range []string{"all", "retried", "three", "invalid"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/lower-logs?source_id=a&limit=25&offset=25&command_attempts="+value, nil))
		if value == "invalid" {
			if response.Code != http.StatusBadRequest {
				t.Fatal(response.Code)
			}
		} else if response.Code != http.StatusOK || service.request.CommandAttempts != value || service.request.Offset != 25 {
			t.Fatalf("status=%d request=%+v", response.Code, service.request)
		}
	}
}

func TestLowerLogIngestAcceptsGzipAndReturnsAck(t *testing.T) {
	service := &lowerLogFakeService{fakeService: &fakeService{}}
	handler := New(service, Options{})
	batch := LowerLogBatch{SourceID: "source-a", BootID: "boot", FirstSequence: 1, Events: []LowerLogEvent{{
		SourceID: "source-a", BootID: "boot", Sequence: 1, Timestamp: time.Now().UTC(), Level: "INFO", Component: "test", Message: "hello",
	}}}
	var body bytes.Buffer
	writer := gzip.NewWriter(&body)
	if err := json.NewEncoder(writer).Encode(batch); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/lower-ingest/logs", &body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.batch.SourceID != "source-a" {
		t.Fatalf("status=%d body=%s batch=%#v", response.Code, response.Body.String(), service.batch)
	}
}

func TestLowerLogIngestMapsSequenceGapToConflict(t *testing.T) {
	service := &lowerLogFakeService{fakeService: &fakeService{}, err: &LowerLogSequenceError{Expected: 3, Actual: 4}}
	handler := New(service, Options{})
	batch := LowerLogBatch{SourceID: "source-a", BootID: "boot", FirstSequence: 4, Events: []LowerLogEvent{{
		SourceID: "source-a", BootID: "boot", Sequence: 4, Timestamp: time.Now().UTC(), Level: "WARN", Message: "gap",
	}}}
	body, _ := json.Marshal(batch)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/lower-ingest/logs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte("log_sequence_gap")) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
