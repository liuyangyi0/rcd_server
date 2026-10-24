package server

import (
	"github.com/awcullen/opcua/ua"
	"testing"
	"time"
)

func TestCommandNotificationsPreserveEqualResultsAndSessionIsolation(t *testing.T) {
	srv := newForkUnitServer()
	id := ua.NewNodeIDString(2, "COM1_Send")
	node := NewVariableNode(srv, id, ua.NewQualifiedName(2, "COM1_Send"), ua.NewLocalizedText("COM1_Send", ""), ua.LocalizedText{}, nil, nil,
		ua.NewDataValue("3", ua.BadTimeout, time.Time{}, 0, time.Now(), 0), ua.DataTypeIDString, ua.ValueRankScalar, nil, ua.AccessLevelsCurrentRead, 0, false, nil)
	if err := srv.namespaceManager.AddNode(node); err != nil {
		t.Fatal(err)
	}
	sender, observer := newForkSession(srv, "sender"), newForkSession(srv, "observer")
	for _, s := range []*Session{sender, observer} {
		if err := srv.sessionManager.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	first := addDisabledMonitoredItem(t, srv, sender, node)
	second := addDisabledMonitoredItem(t, srv, observer, node)
	first.monitoringMode = ua.MonitoringModeReporting
	first.queueSize = 8
	first.discardOldest = true
	first.prequeue.PushBack(ua.NewDataValue("2", ua.Good, time.Time{}, 0, time.Now(), 0))
	for i := 0; i < 3; i++ {
		v := ua.NewDataValue("3", ua.BadTimeout, time.Unix(int64(i+1), 0), 0, time.Now(), 0)
		if count := srv.QueueSessionNodeValue(sender.SessionId(), id, v); count != 1 {
			t.Fatalf("count=%d", count)
		}
	}
	if first.prequeue.Len() != 0 {
		t.Fatal("stale polling sample retained")
	}
	if second.queue.Len() != 0 || second.prequeue.Len() != 0 {
		t.Fatal("result leaked to observer")
	}
	values, more := first.notifications(8)
	if more || len(values) != 3 {
		t.Fatalf("notifications=%d more=%v", len(values), more)
	}
	for i, value := range values {
		v := value.(ua.DataValue)
		if v.Value != "3" || v.StatusCode != ua.BadTimeout || !v.SourceTimestamp.Equal(time.Unix(int64(i+1), 0)) {
			t.Fatalf("notification=%+v", v)
		}
	}
	first.monitoringMode = ua.MonitoringModeDisabled
	srv.QueueSessionNodeValue(sender.SessionId(), id, ua.NewDataValue("1", ua.Good, time.Now(), 0, time.Now(), 0))
	if first.queue.Len() != 0 {
		t.Fatal("disabled monitor received command")
	}
}
