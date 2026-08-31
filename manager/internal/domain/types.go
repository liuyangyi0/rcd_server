package domain

import "time"

// NodeKey is stable across reconnects and namespace-index changes.
type NodeKey struct {
	NamespaceURI string `json:"namespace_uri"`
	Identifier   string `json:"identifier"`
}

type DataType string

const (
	DataTypeBoolean DataType = "Boolean"
	DataTypeUInt32  DataType = "UInt32"
	DataTypeFloat   DataType = "Float"
	DataTypeDouble  DataType = "Double"
	DataTypeString  DataType = "String"
)

type StatusCode uint32

const (
	StatusGood                  StatusCode = 0x00000000
	StatusBadUnexpectedError    StatusCode = 0x80010000
	StatusBadNoCommunication    StatusCode = 0x80310000
	StatusBadWaitingInitialData StatusCode = 0x80320000
	StatusBadConfigurationError StatusCode = 0x80890000
	StatusBadOutOfService       StatusCode = 0x808D0000
	StatusBadWaitingForResponse StatusCode = 0x80B20000
	StatusBadTimeout            StatusCode = 0x800A0000
)

func (s StatusCode) IsGood() bool      { return s&0xC0000000 == 0 }
func (s StatusCode) IsUncertain() bool { return s&0xC0000000 == 0x40000000 }
func (s StatusCode) IsBad() bool       { return s&0xC0000000 == 0x80000000 }

// Sample is a value received from one lower OPC UA source. Callers must treat
// instances as immutable after publication.
type Sample struct {
	Type            DataType   `json:"type"`
	Value           any        `json:"value,omitempty"`
	StatusCode      StatusCode `json:"status_code"`
	SourceTimestamp time.Time  `json:"source_timestamp,omitempty,omitzero"`
	Epoch           uint64     `json:"epoch"`
}

type CatalogState string

const (
	CatalogDiscovering CatalogState = "Discovering"
	CatalogReady       CatalogState = "Ready"
	CatalogCached      CatalogState = "Cached"
	CatalogConflict    CatalogState = "CatalogConflict"
)

func (s CatalogState) Selectable() bool {
	return s == CatalogReady || s == CatalogCached
}

type Group struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	DefaultSourceID string    `json:"default_source_id,omitempty"`
	Revision        uint64    `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Source struct {
	ID            string         `json:"id"`
	GroupID       string         `json:"group_id"`
	GroupBindings []GroupBinding `json:"group_bindings,omitempty"`
	// AvailableGroupIDs is derived from a discovered or cached topology, never
	// accepted as configuration. Nil keeps legacy sources backwards compatible.
	AvailableGroupIDs []string     `json:"available_group_ids,omitempty"`
	Name              string       `json:"name"`
	Endpoint          string       `json:"endpoint"`
	SecurityPolicy    string       `json:"security_policy"`
	SecurityMode      string       `json:"security_mode"`
	Username          string       `json:"username,omitempty"`
	HasPassword       bool         `json:"has_password"`
	Enabled           bool         `json:"enabled"`
	CatalogState      CatalogState `json:"catalog_state"`
	Revision          uint64       `json:"revision"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type NodeClass string

const (
	NodeClassVariable NodeClass = "Variable"
	NodeClassMethod   NodeClass = "Method"
)

type Argument struct {
	Name      string   `json:"name"`
	DataType  DataType `json:"data_type"`
	ValueRank int32    `json:"value_rank"`
}

type CatalogNode struct {
	GroupID         string     `json:"group_id,omitempty"`
	PortID          string     `json:"port_id,omitempty"`
	DeviceID        string     `json:"device_id,omitempty"`
	Kind            string     `json:"kind,omitempty"`
	Key             NodeKey    `json:"key"`
	BrowseName      string     `json:"browse_name"`
	ParentPath      string     `json:"parent_path"`
	Class           NodeClass  `json:"class"`
	DataType        DataType   `json:"data_type,omitempty"`
	ValueRank       int32      `json:"value_rank"`
	Writable        bool       `json:"writable"`
	InputArguments  []Argument `json:"input_arguments,omitempty"`
	OutputArguments []Argument `json:"output_arguments,omitempty"`
	Retired         bool       `json:"retired"`
}

func (n CatalogNode) Clone() CatalogNode {
	n.InputArguments = append([]Argument(nil), n.InputArguments...)
	n.OutputArguments = append([]Argument(nil), n.OutputArguments...)
	return n
}

type SourceCatalog struct {
	SourceID     string          `json:"source_id"`
	GroupID      string          `json:"group_id"`
	State        CatalogState    `json:"state"`
	DiscoveredAt time.Time       `json:"discovered_at"`
	Nodes        []CatalogNode   `json:"nodes"`
	Topology     *SourceTopology `json:"topology,omitempty"`
	// QuarantineReason records an explicitly rejected discovery, unlike a
	// derived conflict between otherwise valid source catalogs. Only a new,
	// successfully validated discovery may clear this durable marker.
	QuarantineReason string `json:"quarantine_reason,omitempty"`
}

func (c SourceCatalog) Clone() SourceCatalog {
	nodes := make([]CatalogNode, len(c.Nodes))
	for i := range c.Nodes {
		nodes[i] = c.Nodes[i].Clone()
	}
	c.Nodes = nodes
	c.Topology = c.Topology.Clone()
	return c
}

type GroupRoute struct {
	GroupID  string `json:"group_id"`
	SourceID string `json:"source_id"`
	Revision uint64 `json:"revision"`
}

type SessionRouteChange struct {
	SessionID string     `json:"session_id"`
	Route     GroupRoute `json:"route"`
}

type Session struct {
	ID          string                `json:"id"`
	ActivatedAt time.Time             `json:"activated_at"`
	LastSeenAt  time.Time             `json:"last_seen_at"`
	Routes      map[string]GroupRoute `json:"routes"`
}

type CommandState string

const (
	CommandWaiting  CommandState = "Waiting"
	CommandReceived CommandState = "Received"
	CommandFailed   CommandState = "Failed"
	CommandTimedOut CommandState = "TimedOut"
)

type PendingCommand struct {
	ID         string       `json:"id"`
	SessionID  string       `json:"session_id"`
	GroupID    string       `json:"group_id"`
	SourceID   string       `json:"source_id"`
	Port       string       `json:"port"`
	Kind       string       `json:"kind"`
	State      CommandState `json:"state"`
	StatusCode StatusCode   `json:"status_code"`
	Error      string       `json:"error,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at,omitempty,omitzero"`
}

type AuditEntry struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	Actor      string         `json:"actor"`
	Target     string         `json:"target"`
	Revision   uint64         `json:"revision"`
	Details    map[string]any `json:"details,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type AlarmSeverity string

const (
	AlarmSeverityWarning  AlarmSeverity = "Warning"
	AlarmSeverityCritical AlarmSeverity = "Critical"
)

type AlarmState string

const (
	AlarmOpen     AlarmState = "Open"
	AlarmResolved AlarmState = "Resolved"
)

// Alarm is a persistent, low-frequency operational condition. It deliberately
// does not contain point values and therefore stays outside the real-time data
// path.
type Alarm struct {
	ID             int64         `json:"id"`
	SourceID       string        `json:"source_id,omitempty"`
	Kind           string        `json:"kind"`
	Severity       AlarmSeverity `json:"severity"`
	State          AlarmState    `json:"state"`
	Message        string        `json:"message"`
	Occurrences    uint64        `json:"occurrences"`
	AcknowledgedAt time.Time     `json:"acknowledged_at,omitempty,omitzero"`
	AcknowledgedBy string        `json:"acknowledged_by,omitempty"`
	SilencedUntil  time.Time     `json:"silenced_until,omitempty,omitzero"`
	OpenedAt       time.Time     `json:"opened_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ResolvedAt     time.Time     `json:"resolved_at,omitempty,omitzero"`
	Revision       uint64        `json:"revision"`
}

// SourceEvent records a state transition for troubleshooting and uptime
// reconstruction. Only control-plane state is persisted.
type SourceEvent struct {
	ID         int64     `json:"id"`
	SourceID   string    `json:"source_id"`
	Kind       string    `json:"kind"`
	Previous   string    `json:"previous,omitempty"`
	Current    string    `json:"current"`
	Message    string    `json:"message,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}
