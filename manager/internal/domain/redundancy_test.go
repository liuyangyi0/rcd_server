package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestRedundancyComparison(t *testing.T) {
	p := DefaultRedundancyPolicy("g")
	p.AbsoluteTolerance = 1
	p.RelativeTolerance = .01
	at := time.Now()
	a := Sample{Type: DataTypeDouble, Value: 100.0, SourceTimestamp: at}
	b := a
	b.Value = 100.9
	if different, ok := CompareSamples(a, b, p); different || !ok {
		t.Fatal("within tolerance")
	}
	b.Value = 102.0
	if different, ok := CompareSamples(a, b, p); !different || !ok {
		t.Fatal("difference not detected")
	}
	b.StatusCode = StatusBadNoCommunication
	if _, ok := CompareSamples(a, b, p); ok {
		t.Fatal("bad quality compared")
	}
	b = a
	b.SourceTimestamp = at.Add(3 * time.Second)
	if _, ok := CompareSamples(a, b, p); ok {
		t.Fatal("skew compared")
	}
	b = a
	b.SourceTimestamp = time.Time{}
	if _, ok := CompareSamples(a, b, p); ok {
		t.Fatal("unknown time compared")
	}
	b = a
	b.Value = math.NaN()
	if _, ok := CompareSamples(a, b, p); ok {
		t.Fatal("NaN compared")
	}
	a.Type = DataTypeBoolean
	a.Value = true
	b = a
	b.Value = false
	if d, ok := CompareSamples(a, b, p); !d || !ok {
		t.Fatal("boolean difference")
	}
	p.Enabled = true
	p.Priority = []string{"a", "a"}
	if p.Validate() == nil {
		t.Fatal("duplicate accepted")
	}
	p.Priority = []string{"a", "b"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.RelativeTolerance = math.Inf(1)
	if p.Validate() == nil {
		t.Fatal("infinite tolerance accepted")
	}
}
func TestAutomaticRouteIsolationAndCommandFence(t *testing.T) {
	rt, err := NewRouteTable([]Group{{ID: "g", DefaultSourceID: "a"}}, []Source{{ID: "a", GroupID: "g", Enabled: true, CatalogState: CatalogReady}, {ID: "b", GroupID: "g", Enabled: true, CatalogState: CatalogReady}})
	if err != nil {
		t.Fatal(err)
	}
	rt.OpenSessionWithAutomatic("auto", map[string]bool{"g": true})
	rt.OpenSession("manual")
	moved, err := rt.SelectAutomatic("auto", "g", "b", 1)
	if err != nil || !moved.Automatic {
		t.Fatal(moved, err)
	}
	restored, _ := rt.OpenSessionWithAutomatic("auto", nil)
	if !restored.Routes["g"].Automatic || restored.Routes["g"].SourceID != "b" {
		t.Fatal("reattach reset route")
	}
	manual, _ := rt.Route("manual", "g")
	if manual.SourceID != "a" || manual.Automatic {
		t.Fatal(manual)
	}
	if _, err = rt.SetAutomatic("auto", "g", false, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal(err)
	}
	_, err = rt.BeginCommand("auto", "g", "port", "Write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rt.SelectAutomatic("auto", "g", "a", moved.Revision); !errors.Is(err, ErrGroupPending) {
		t.Fatal(err)
	}
	if _, err = rt.SetAutomatic("manual", "g", true, 1); !errors.Is(err, ErrGroupPending) {
		t.Fatal(err)
	}
	rt.CloseSession("auto")
	fresh, _ := rt.OpenSession("auto")
	if fresh.Routes["g"].Automatic || fresh.Routes["g"].SourceID != "a" {
		t.Fatal("new Session retained mode")
	}
}
