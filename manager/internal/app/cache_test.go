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

func TestSampleCacheRejectsOlderTimestampWithinEpoch(t *testing.T) {
	cache := newSampleCache()
	key := domain.NodeKey{NamespaceURI: "urn:test", Identifier: "COM1_Send"}
	newer := time.Now().UTC()
	if !cache.Put("source", key, domain.Sample{Value: "1", Epoch: 2, SourceTimestamp: newer}) {
		t.Fatal("new terminal sample was rejected")
	}
	if cache.Put("source", key, domain.Sample{Value: "2", Epoch: 2, SourceTimestamp: newer.Add(-time.Second)}) {
		t.Fatal("late waiting sample was accepted")
	}
	got, _ := cache.Get("source", key)
	if got.Value != "1" {
		t.Fatalf("cached value regressed to %#v", got.Value)
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
