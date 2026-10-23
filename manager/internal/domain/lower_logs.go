package domain

import (
	"fmt"
	"time"
)

type LowerLogEvent struct {
	SourceID  string         `json:"source_id"`
	BootID    string         `json:"boot_id"`
	Sequence  uint64         `json:"sequence"`
	Timestamp time.Time      `json:"timestamp"`
	Level     string         `json:"level"`
	Component string         `json:"component"`
	PortID    string         `json:"port_id,omitempty"`
	DeviceID  string         `json:"device_id,omitempty"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type LowerLogBatch struct {
	SourceID      string          `json:"source_id"`
	BootID        string          `json:"boot_id"`
	FirstSequence uint64          `json:"first_sequence"`
	Events        []LowerLogEvent `json:"events"`
}

type LowerLogAck struct {
	SourceID        string    `json:"source_id"`
	BootID          string    `json:"boot_id"`
	AcceptedThrough uint64    `json:"accepted_through"`
	ReceivedAt      time.Time `json:"received_at"`
}

type LowerLogRequest struct {
	CommandAttempts string
	SourceID        string
	Level           string
	PortID          string
	DeviceID        string
	Query           string
	From            time.Time
	To              time.Time
	Limit           int
	Offset          int
}

type LowerLogItem struct {
	LowerLogEvent
	ReceivedAt time.Time `json:"received_at"`
}

type LowerLogPage struct {
	Items          []LowerLogItem `json:"items"`
	Total          int            `json:"total"`
	Limit          int            `json:"limit"`
	Offset         int            `json:"offset"`
	InvalidLines   int            `json:"invalid_lines,omitempty"`
	LastReceivedAt time.Time      `json:"last_received_at,omitempty,omitzero"`
	PushState      string         `json:"push_state"`
}

type LowerLogSequenceError struct {
	Expected uint64
	Actual   uint64
}

func (e *LowerLogSequenceError) Error() string {
	return fmt.Sprintf("log sequence gap: expected %d, received %d", e.Expected, e.Actual)
}
