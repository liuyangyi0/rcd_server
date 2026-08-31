package opc

import (
	"testing"

	gopcua "github.com/gopcua/opcua"
	gu "github.com/gopcua/opcua/ua"
)

func TestLiveGroupsGoodSampleMatching(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  gu.StatusCode
		value   any
		handle  uint32
		matches bool
	}{
		{"plain Good", gu.StatusOK, uint32(101), 7, true},
		{"Rust Good with Overflow", 0x00000080, uint32(101), 7, true},
		{"Good DataValue Overflow", 0x00000480, float64(101), 7, true},
		{"Good semantic change", 0x00004000, float64(101), 7, true},
		{"Good subcode", gu.StatusGoodClamped, uint32(101), 7, true},
		{"Uncertain is not Good", 0x40000080, uint32(101), 7, false},
		{"Bad is not Good", gu.StatusBadNoCommunication, uint32(101), 7, false},
		{"reserved severity is not Good", 0xC0000080, uint32(101), 7, false},
		{"wrong numeric value", 0x00000080, uint32(11), 7, false},
		{"nonnumeric value", 0x00000080, "not a number", 7, false},
		{"different node handle", 0x00000080, uint32(101), 8, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := &gu.DataValue{Status: test.status, Value: gu.MustVariant(test.value)}
			item := &gu.MonitoredItemNotification{ClientHandle: test.handle, Value: value}
			if got := liveGroupsMonitoredValueMatches(item, 7, 101); got != test.matches {
				t.Fatalf("handle=%d %s: matched=%v want=%v", test.handle, liveGroupsDescribeValue(value), got, test.matches)
			}
			if test.handle == 7 {
				if got := liveGroupsNumberMatches(value, 101); got != test.matches {
					t.Fatalf("Read sample matching differs: got=%v want=%v", got, test.matches)
				}
			}
			if value.Status != test.status {
				t.Fatal("matching must not strip status information bits")
			}
		})
	}
	for _, item := range []*gu.MonitoredItemNotification{nil, {ClientHandle: 7}, {ClientHandle: 7, Value: &gu.DataValue{Status: gu.StatusOK}}} {
		if liveGroupsMonitoredValueMatches(item, 7, 101) {
			t.Fatalf("missing notification/DataValue/Variant matched: %#v", item)
		}
	}
}

func TestLiveGroupsExplicitQualityRemainsExact(t *testing.T) {
	for _, status := range []gu.StatusCode{
		gu.StatusBadNoCommunication,
		gu.StatusBadNoCommunication | 0x80,
		gu.StatusBadOutOfService,
		gu.StatusBadConfigurationError,
		0x40000080,
		0x00000080,
	} {
		value := &gu.DataValue{Status: status, Value: gu.MustVariant(uint32(101))}
		if got, want := liveGroupsDataValueHasStatus(value, gu.StatusBadNoCommunication), status == gu.StatusBadNoCommunication; got != want {
			t.Fatalf("expected exact BadNoCommunication, actual=0x%08X matched=%v want=%v", uint32(status), got, want)
		}
		if value.Status != status {
			t.Fatal("quality comparison changed the source status")
		}
	}
}

func TestLiveGroupsAwaitValueGoodInformationBits(t *testing.T) {
	// Exercise the actual subscription wait path with the nonzero Good code
	// seen in the real Rust fixture, without a network or environment opt-in.
	notifications := make(chan *gopcua.PublishNotificationData, 1)
	notifications <- &gopcua.PublishNotificationData{Value: &gu.DataChangeNotification{
		MonitoredItems: []*gu.MonitoredItemNotification{{ClientHandle: 7, Value: &gu.DataValue{
			Status: 0x00000080,
			Value:  gu.MustVariant(uint32(101)),
		}}},
	}}
	close(notifications)
	liveGroupsAwaitValue(t, notifications, 7, 101)
}
