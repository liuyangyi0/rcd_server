package opc

import (
	"context"
	"errors"
	"testing"
	"time"

	awu "github.com/awcullen/opcua/ua"
	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/domain"
)

type commandRecord struct {
	sessionID string
	key       domain.NodeKey
	value     any
	hexData   string
}

type fakeBackend struct {
	samples     map[string]domain.Sample
	records     []commandRecord
	groups      []domain.Group
	sources     []domain.Source
	session     domain.Session
	writeStatus domain.StatusCode
	writeErr    error
	callStatus  domain.StatusCode
	callErr     error
}

func (f *fakeBackend) OpenSession(string) error  { return nil }
func (f *fakeBackend) CloseSession(string)       {}
func (f *fakeBackend) TouchSession(string) error { return nil }
func (f *fakeBackend) ReadSample(sessionID string, _ domain.NodeKey) domain.Sample {
	return f.samples[sessionID]
}
func (f *fakeBackend) WriteCommand(_ context.Context, sessionID string, key domain.NodeKey, value any) (domain.StatusCode, error) {
	f.records = append(f.records, commandRecord{sessionID: sessionID, key: key, value: value})
	return f.writeStatus, f.writeErr
}
func (f *fakeBackend) CallCommand(_ context.Context, sessionID string, key domain.NodeKey, hexData string) (string, domain.StatusCode, error) {
	f.records = append(f.records, commandRecord{sessionID: sessionID, key: key, hexData: hexData})
	return "queued", f.callStatus, f.callErr
}
func (f *fakeBackend) SelectSource(string, string, string, uint64) (domain.GroupRoute, error) {
	return domain.GroupRoute{}, nil
}
func (f *fakeBackend) SetDefaultSource(string, string, uint64) (domain.Group, error) {
	return domain.Group{}, nil
}
func (f *fakeBackend) Session(string) (domain.Session, error)       { return f.session, nil }
func (f *fakeBackend) Groups() []domain.Group                       { return f.groups }
func (f *fakeBackend) Sources() []domain.Source                     { return f.sources }
func (f *fakeBackend) LowerStatus(string) (LowerStatus, bool)       { return LowerStatus{}, false }
func (f *fakeBackend) SourcePointStatistics(string) PointStatistics { return PointStatistics{} }
func (f *fakeBackend) GroupForNode(string) (string, bool)           { return "g", true }
func (f *fakeBackend) PendingCount(string) int                      { return 0 }

func TestSampleFromGopcuaPreservesStatusSourceTimeAndEpoch(t *testing.T) {
	sourceTime := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	value := &gu.DataValue{
		EncodingMask: gu.DataValueValue | gu.DataValueStatusCode | gu.DataValueSourceTimestamp,
		Value:        gu.MustVariant(uint32(42)), Status: gu.StatusBadNoCommunication,
		SourceTimestamp: sourceTime,
	}
	sample := sampleFromGopcua(value, domain.DataTypeUInt32, 17)
	if sample.Value != uint32(42) || sample.Type != domain.DataTypeUInt32 {
		t.Fatalf("sample value/type = %#v", sample)
	}
	if sample.StatusCode != domain.StatusCode(gu.StatusBadNoCommunication) {
		t.Fatalf("status = %#x", sample.StatusCode)
	}
	if !sample.SourceTimestamp.Equal(sourceTime) || sample.Epoch != 17 {
		t.Fatalf("time/epoch not preserved: %#v", sample)
	}
}

func TestSampleToDataValuePreservesSourceAndSetsManagerServerTime(t *testing.T) {
	sourceTime := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	serverTime := sourceTime.Add(250 * time.Millisecond)
	dv := sampleToDataValue(domain.Sample{
		Type: domain.DataTypeDouble, Value: float64(12.5),
		StatusCode: domain.StatusBadNoCommunication, SourceTimestamp: sourceTime,
	}, domain.DataTypeDouble, awu.ValueRankScalar, serverTime)
	if dv.Value != float64(12.5) || dv.StatusCode != awu.BadNoCommunication {
		t.Fatalf("data value = %#v", dv)
	}
	if !dv.SourceTimestamp.Equal(sourceTime) || !dv.ServerTimestamp.Equal(serverTime) {
		t.Fatalf("timestamps = source %s server %s", dv.SourceTimestamp, dv.ServerTimestamp)
	}
}

func TestSampleToDataValueUsesTypedZeroForMissingValue(t *testing.T) {
	dv := sampleToDataValue(domain.Sample{StatusCode: domain.StatusBadNoCommunication}, domain.DataTypeUInt32, awu.ValueRankScalar, time.Now())
	if value, ok := dv.Value.(uint32); !ok || value != 0 {
		t.Fatalf("typed zero = %#v (%T)", dv.Value, dv.Value)
	}
}

func TestSampleToDataValueUsesTypedEmptyArrayForMissingValue(t *testing.T) {
	dv := sampleToDataValue(domain.Sample{StatusCode: domain.StatusBadNoCommunication}, domain.DataTypeDouble, awu.ValueRankOneDimension, time.Now())
	value, ok := dv.Value.([]float64)
	if !ok || len(value) != 0 {
		t.Fatalf("typed empty array = %#v (%T)", dv.Value, dv.Value)
	}
}

func TestReadBusinessValueIsSessionScoped(t *testing.T) {
	backend := &fakeBackend{samples: map[string]domain.Sample{
		"left":  {Type: domain.DataTypeUInt32, Value: uint32(1), StatusCode: domain.StatusGood},
		"right": {Type: domain.DataTypeUInt32, Value: uint32(2), StatusCode: domain.StatusGood},
	}}
	spec := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "KKS"}, DataType: domain.DataTypeUInt32, ValueRank: awu.ValueRankScalar}
	left := readBusinessValue(backend, "left", spec, false, time.Now())
	right := readBusinessValue(backend, "right", spec, false, time.Now())
	if left.Value != uint32(1) || right.Value != uint32(2) {
		t.Fatalf("session values leaked: left=%v right=%v", left.Value, right.Value)
	}
}

func TestRetiredBusinessValueOverridesBackend(t *testing.T) {
	backend := &fakeBackend{samples: map[string]domain.Sample{"s": {Value: uint32(99), StatusCode: domain.StatusGood}}}
	spec := domain.CatalogNode{Key: domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "KKS"}, DataType: domain.DataTypeUInt32, ValueRank: awu.ValueRankScalar}
	dv := readBusinessValue(backend, "s", spec, true, time.Now())
	if dv.StatusCode != awu.BadOutOfService || dv.Value != uint32(0) {
		t.Fatalf("retired value = %#v", dv)
	}
}

func TestCommandProxyPassesImmutableCallSnapshot(t *testing.T) {
	backend := &fakeBackend{writeStatus: domain.StatusGood, callStatus: domain.StatusGood}
	key := domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_Send"}
	_, writeStatus := writeBusinessCommand(context.Background(), backend, "session-a", key, "AABB", time.Now())
	methodKey := domain.NodeKey{NamespaceURI: BusinessNamespaceURI, Identifier: "COM1_SendCommand"}
	call := callBusinessCommand(context.Background(), backend, "session-a", methodKey, "0102")
	if writeStatus != awu.Good || call.StatusCode != awu.Good || len(backend.records) != 2 {
		t.Fatalf("command results: write=%v call=%#v records=%#v", writeStatus, call, backend.records)
	}
	if backend.records[0].sessionID != "session-a" || backend.records[0].key != key || backend.records[0].value != "AABB" {
		t.Fatalf("write snapshot changed: %#v", backend.records[0])
	}
	if backend.records[1].key != methodKey || backend.records[1].hexData != "0102" {
		t.Fatalf("call snapshot changed: %#v", backend.records[1])
	}
}

func TestStatusFromBackendMapsPendingAndRevision(t *testing.T) {
	if got := statusFromBackend(domain.StatusGood, domain.ErrGroupPending); got != awu.BadWaitingForResponse {
		t.Fatalf("pending = %v", got)
	}
	if got := statusFromBackend(domain.StatusGood, domain.ErrRevisionConflict); got != awu.BadInvalidState {
		t.Fatalf("revision = %v", got)
	}
	if got := statusFromBackend(domain.StatusGood, context.DeadlineExceeded); got != awu.BadTimeout {
		t.Fatalf("timeout = %v", got)
	}
	if got := statusFromBackend(domain.StatusGood, errors.New("boom")); got != awu.BadUnexpectedError {
		t.Fatalf("unknown = %v", got)
	}
}
