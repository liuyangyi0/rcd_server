package opc

import (
	awu "github.com/awcullen/opcua/ua"
	"rcd_manager/internal/domain"
)

// Optional extension keeps older/testing backends compatible.
type RedundancyBackend interface {
	GroupRedundancy(string) (domain.RedundancyView, error)
	SetSessionAutomatic(string, string, bool, uint64) (domain.GroupRoute, error)
}

func (s *UpstreamServer) redundancyVariables(group string) []managementVariable {
	backend, ok := s.backend.(RedundancyBackend)
	if !ok {
		return nil
	}
	read := func(field string) func(string) (any, awu.StatusCode) {
		return func(_ string) (any, awu.StatusCode) {
			v, err := backend.GroupRedundancy(group)
			if err != nil {
				return nil, statusFromBackend(domain.StatusGood, err)
			}
			switch field {
			case "AutoSwitchEnabled":
				return v.Policy.Enabled, awu.Good
			case "DefaultAutomatic":
				return v.Policy.DefaultAutomatic && v.Policy.Enabled, awu.Good
			case "DataDifferenceAlarmActive":
				return v.DifferenceAlarmActive, awu.Good
			case "DataDifferenceCount":
				return uint32(v.DifferenceCount), awu.Good
			case "DataComparisonState":
				return v.ComparisonState, awu.Good
			case "LastAutoSwitchReason":
				return v.LastSwitchReason, awu.Good
			}
			return "", awu.Good
		}
	}
	defs := []managementVariable{}
	for _, field := range []string{"AutoSwitchEnabled", "DefaultAutomatic", "DataDifferenceAlarmActive", "DataDifferenceCount", "DataComparisonState", "LastAutoSwitchReason"} {
		typ := awu.DataTypeIDString
		if field == "AutoSwitchEnabled" || field == "DefaultAutomatic" || field == "DataDifferenceAlarmActive" {
			typ = awu.DataTypeIDBoolean
		}
		if field == "DataDifferenceCount" {
			typ = awu.DataTypeIDUInt32
		}
		defs = append(defs, managementVariable{name: field, dataType: typ, valueRank: awu.ValueRankScalar, minimumSamplingInterval: 1000, read: read(field)})
	}
	defs = append(defs, managementVariable{name: "AutomaticSelection", dataType: awu.DataTypeIDBoolean, valueRank: awu.ValueRankScalar, writable: true,
		read: func(sid string) (any, awu.StatusCode) {
			session, err := s.backend.Session(sid)
			if err != nil {
				return false, statusFromBackend(domain.StatusGood, err)
			}
			route, ok := session.Routes[group]
			if !ok {
				return false, awu.BadNotFound
			}
			return route.Automatic, awu.Good
		},
		write: func(sid string, value any) awu.StatusCode {
			automatic, ok := value.(bool)
			if !ok {
				return awu.BadTypeMismatch
			}
			session, err := s.backend.Session(sid)
			if err != nil {
				return statusFromBackend(domain.StatusGood, err)
			}
			route, ok := session.Routes[group]
			if !ok {
				return awu.BadNotFound
			}
			_, err = backend.SetSessionAutomatic(sid, group, automatic, route.Revision)
			return statusFromBackend(domain.StatusGood, err)
		},
	})
	return defs
}
