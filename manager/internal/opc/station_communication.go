package opc

import (
	"time"

	awu "github.com/awcullen/opcua/ua"
	"rcd_manager/internal/domain"
)

// Optional to preserve compatibility with alternate upstream backends.
type StationCommunicationBackend interface {
	StationCommunication(sourceID, portID, deviceID string) domain.StationCommunication
}

func stationCommunicationVariables(backend UpstreamBackend, sourceID, portID, deviceID string) []managementVariable {
	lookup := func() domain.StationCommunication {
		if provider, ok := backend.(StationCommunicationBackend); ok {
			return provider.StationCommunication(sourceID, portID, deviceID)
		}
		return domain.StationCommunication{}
	}
	return []managementVariable{
		{name: "CommunicationState", dataType: awu.DataTypeIDUInt32, valueRank: awu.ValueRankScalar,
			read: func(string) (any, awu.StatusCode) { return lookup().State, awu.Good }},
		{name: "CommunicationStateText", dataType: awu.DataTypeIDString, valueRank: awu.ValueRankScalar,
			read: func(string) (any, awu.StatusCode) { return lookup().Text(), awu.Good }},
		{name: "CommunicationObservedAt", dataType: awu.DataTypeIDDateTime, valueRank: awu.ValueRankScalar,
			read: func(string) (any, awu.StatusCode) {
				observed := lookup().ObservedAt
				if observed.IsZero() {
					return time.Time{}, awu.BadWaitingForInitialData
				}
				return observed, awu.Good
			}},
	}
}
