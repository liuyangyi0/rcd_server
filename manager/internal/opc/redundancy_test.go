package opc

import (
	awu "github.com/awcullen/opcua/ua"
	"rcd_manager/internal/domain"
	"testing"
)

type redundancyBackendStub struct {
	*fakeBackend
	routes  map[string]domain.GroupRoute
	policy  domain.RedundancyPolicy
	pending bool
}

func (b *redundancyBackendStub) GroupRedundancy(string) (domain.RedundancyView, error) {
	return domain.RedundancyView{Policy: b.policy, DifferenceCount: 2, ComparisonState: "Different"}, nil
}
func (b *redundancyBackendStub) Session(id string) (domain.Session, error) {
	route, ok := b.routes[id]
	if !ok {
		return domain.Session{}, domain.ErrSessionClosed
	}
	return domain.Session{ID: id, Routes: map[string]domain.GroupRoute{"g": route}}, nil
}
func (b *redundancyBackendStub) SetSessionAutomatic(id, group string, a bool, revision uint64) (domain.GroupRoute, error) {
	if b.pending {
		return domain.GroupRoute{}, domain.ErrGroupPending
	}
	route := b.routes[id]
	if route.Revision != revision {
		return domain.GroupRoute{}, domain.ErrRevisionConflict
	}
	route.Automatic = a
	route.Revision++
	b.routes[id] = route
	return route, nil
}
func TestRedundancyOPCContractAndSessionIsolation(t *testing.T) {
	b := &redundancyBackendStub{fakeBackend: &fakeBackend{}, routes: map[string]domain.GroupRoute{"a": {GroupID: "g", Revision: 1}, "b": {GroupID: "g", Revision: 1}}, policy: domain.DefaultRedundancyPolicy("g")}
	s := &UpstreamServer{backend: b}
	defs := s.redundancyVariables("g")
	mode := stationDefinition(t, defs, "AutomaticSelection")
	if !mode.writable || mode.dataType != awu.DataTypeIDBoolean {
		t.Fatal("bad mode shape")
	}
	if got := mode.write("a", "true"); got != awu.BadTypeMismatch {
		t.Fatal(got)
	}
	if got := mode.write("a", true); got != awu.Good {
		t.Fatal(got)
	}
	if a, _ := mode.read("a"); a != true {
		t.Fatal(a)
	}
	if v, _ := mode.read("b"); v != false {
		t.Fatal("session leak", v)
	}
	b.pending = true
	if got := mode.write("b", true); got != awu.BadWaitingForResponse {
		t.Fatal(got)
	}
	for _, name := range []string{"AutoSwitchEnabled", "DefaultAutomatic", "DataDifferenceAlarmActive", "DataDifferenceCount", "DataComparisonState", "LastAutoSwitchReason"} {
		d := stationDefinition(t, defs, name)
		if d.writable || d.write != nil {
			t.Fatal("writable diagnostic", name)
		}
	}
	count := stationDefinition(t, defs, "DataDifferenceCount")
	if value, status := count.read("b"); value != uint32(2) || status != awu.Good {
		t.Fatal(value, status)
	}
}
