package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type CommandAttempt struct {
	Attempt uint32 `json:"attempt"`
	Sent    bool   `json:"sent"`
	Result  string `json:"result"`
	At      string `json:"at"`
}
type TrackedCommand struct {
	SchemaVersion     uint32           `json:"schema_version"`
	ManagerInstanceID string           `json:"manager_instance_id"`
	CommandID         string           `json:"command_id"`
	GatewayInstanceID string           `json:"gateway_instance_id"`
	BootID            string           `json:"boot_id"`
	PortID            string           `json:"port_id"`
	AckID             string           `json:"ack_id"`
	RequestHash       string           `json:"request_hash"`
	RequestPreview    string           `json:"request_preview"`
	DeviceID          string           `json:"device_id"`
	Accepted          bool             `json:"accepted"`
	StatusCode        StatusCode       `json:"status_code"`
	Phase             string           `json:"phase"`
	AttemptCount      uint32           `json:"attempt_count"`
	SentCount         uint32           `json:"sent_count"`
	Attempts          []CommandAttempt `json:"attempts"`
	Error             string           `json:"error"`
	Revision          uint64           `json:"revision"`
	StartedAt         string           `json:"started_at"`
	UpdatedAt         string           `json:"updated_at"`
	AuditIncomplete   bool             `json:"audit_incomplete"`
}

func (v TrackedCommand) Validate() error {
	if v.SchemaVersion != 1 || v.CommandID == "" || v.ManagerInstanceID == "" || v.GatewayInstanceID == "" || v.BootID == "" || v.PortID == "" ||
		v.Revision == 0 || v.Revision > 9007199254740991 || v.SentCount > v.AttemptCount || len(v.Attempts) > 10000 || len(v.RequestHash) != 64 {
		return fmt.Errorf("invalid tracked command snapshot")
	}
	if _, err := hex.DecodeString(v.RequestHash); err != nil {
		return fmt.Errorf("invalid request hash")
	}
	var previous uint32
	for _, attempt := range v.Attempts {
		if attempt.Attempt <= previous || attempt.Attempt > v.AttemptCount {
			return fmt.Errorf("invalid attempt sequence")
		}
		previous = attempt.Attempt
		if _, err := time.Parse(time.RFC3339Nano, attempt.At); err != nil {
			return err
		}
	}
	switch v.Phase {
	case "Accepted", "Executing", "AckReceived", "Failed", "Expired", "Unknown", "SentUnconfirmed":
	default:
		return fmt.Errorf("invalid command phase")
	}
	if _, err := time.Parse(time.RFC3339Nano, v.UpdatedAt); err != nil {
		return err
	}
	return nil
}
func (v TrackedCommand) Terminal() bool {
	return v.Phase == "AckReceived" || v.Phase == "Failed" || v.Phase == "Expired" || v.Phase == "Unknown" || v.Phase == "SentUnconfirmed"
}
func CommandRequestHash(port, payload string) string {
	normalized := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, payload))
	return fmt.Sprintf("%x", sha256.Sum256([]byte(port+"\x00"+normalized)))
}

type CommandTrackingHealth struct {
	GatewayInstanceID string `json:"gateway_instance_id"`
	BootID            string `json:"boot_id"`
	Revision          string `json:"revision"`
	AuditDegraded     bool   `json:"audit_degraded"`
	AuditError        string `json:"audit_error"`
	HistoryGap        bool   `json:"history_gap"`
}
type CommandEventBatch struct {
	Items          []TrackedCommand      `json:"items"`
	NextCursor     string                `json:"next_cursor"`
	OldestRevision string                `json:"oldest_revision"`
	HistoryGap     bool                  `json:"history_gap"`
	Health         CommandTrackingHealth `json:"health"`
}
type CommandTrace struct {
	Protocol          string          `json:"protocol"`
	GatewayInstanceID string          `json:"gateway_instance_id"`
	BootID            string          `json:"boot_id"`
	PhysicalPort      string          `json:"physical_port"`
	RequestHash       string          `json:"request_hash"`
	Snapshot          *TrackedCommand `json:"snapshot,omitempty"`
	HistoryGap        bool            `json:"history_gap"`
}
type CommandDetail struct {
	Command PendingCommand `json:"command"`
	Trace   *CommandTrace  `json:"trace,omitempty"`
}

// Nil counts mean unknown, never zero inferred from missing evidence.
type CommandTrackingSummary struct {
	Protocol        string  `json:"protocol"`
	Phase           string  `json:"phase"`
	AttemptCount    *uint32 `json:"attempt_count,omitempty"`
	SentCount       *uint32 `json:"sent_count,omitempty"`
	AuditIncomplete bool    `json:"audit_incomplete"`
	HistoryGap      bool    `json:"history_gap"`
}
