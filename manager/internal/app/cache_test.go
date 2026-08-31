package app

import (
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestSampleCacheRejectsStaleEpoch(t *testing.T) {
	cache := newSampleCache()
	key := domain.NodeKey{NamespaceURI: "urn:test", Identifier: "point"}
	if !cache.Put("source", key, domain.Sample{Value: uint32(2), Epoch: 2}) {
		t.Fatal("initial Put() was rejected")
	}
	if cache.Put("source", key, domain.Sample{Value: uint32(1), Epoch: 1}) {
		t.Fatal("stale Put() was accepted")
	}
	got, ok := cache.Get("source", key)
	if !ok || got.Value != uint32(2) {
		t.Fatalf("Get() = %#v, %v", got, ok)
	}
}

func TestSampleCachePreservesValueAndQualityWhenSourceClockMovesBack(t *testing.T) {
	cache := newSampleCache()
	key := domain.NodeKey{NamespaceURI: "urn:test", Identifier: "KKS001"}
	newer := time.Now().UTC()
	if !cache.Put("source", key, domain.Sample{Value: float64(10), StatusCode: domain.StatusGood, Epoch: 2, SourceTimestamp: newer}) {
		t.Fatal("initial sample was rejected")
	}
	older := newer.Add(-time.Hour)
	if !cache.Put("source", key, domain.Sample{Value: float64(-1), StatusCode: domain.StatusBadOutOfService, Epoch: 2, SourceTimestamp: older}) {
		t.Fatal("current-epoch sample was rejected because the source clock moved backwards")
	}
	got, _ := cache.Get("source", key)
	if got.Value != float64(-1) || got.StatusCode != domain.StatusBadOutOfService || !got.SourceTimestamp.Equal(older) {
		t.Fatalf("cache did not preserve the latest received value/quality/time: %+v", got)
	}
}

func TestZeroValueUsesOPCBuiltinType(t *testing.T) {
	if got := zeroValue(domain.DataTypeFloat); got != float32(0) {
		t.Fatalf("Float zero = %#v", got)
	}
	if got := zeroValue(domain.DataTypeDouble); got != float64(0) {
		t.Fatalf("Double zero = %#v", got)
	}
}
