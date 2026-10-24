package domain

import "time"

// These values are a public OPC contract, not UA DataValue quality codes.
const (
	StationCommunicationUnknown uint32 = iota
	StationCommunicationGood
	StationCommunicationFailed
	StationCommunicationPortClosed
	StationCommunicationDisabled
	StationCommunicationLowerOffline
)

const StationCommunicationMaxAge = 15 * time.Second

type StationCommunication struct {
	State      uint32
	ObservedAt time.Time
}

func (s StationCommunication) Text() string {
	switch s.State {
	case StationCommunicationGood:
		return "通信正常"
	case StationCommunicationFailed:
		return "通信异常（超时或无有效数据）"
	case StationCommunicationPortClosed:
		return "串口未打开"
	case StationCommunicationDisabled:
		return "串口或站点已停用"
	case StationCommunicationLowerOffline:
		return "下级 OPC 离线"
	default:
		return "状态未知（尚未获取、正在切换或状态已过期）"
	}
}

// Only a fresh snapshot from the current lower connection may describe a
// station. A false communication_ok does not prove a hardware timeout: older
// lowers combine timeout, invalid frames and no initial data in that boolean.
func ResolveStationCommunication(online, fresh bool, observedAt, now time.Time, port StationVersionPort, device StationVersion) StationCommunication {
	result := StationCommunication{ObservedAt: observedAt}
	if !online {
		result.State = StationCommunicationLowerOffline
		return result
	}
	if !fresh || observedAt.IsZero() || now.Before(observedAt) || now.Sub(observedAt) > StationCommunicationMaxAge {
		return result
	}
	switch {
	case !port.Enabled || !device.ReadEnabled:
		result.State = StationCommunicationDisabled
	case port.ControlPending:
		result.State = StationCommunicationUnknown
	case !port.Open:
		result.State = StationCommunicationPortClosed
	case !port.AppliedEnabled || !device.AppliedReadEnabled:
		result.State = StationCommunicationUnknown
	case device.CommunicationOK:
		result.State = StationCommunicationGood
	default:
		result.State = StationCommunicationFailed
	}
	return result
}
