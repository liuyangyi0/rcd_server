package opc

import (
	"context"
	"errors"
	"time"

	"rcd_manager/internal/domain"
)

const (
	BusinessNamespaceURI    = "urn:SerialServer"
	TopologyNamespaceURI    = "urn:SerialServer:topology"
	ApplicationNamespaceURI = "urn:RcdManager:rcd_manager"
	ManagementNamespaceURI  = "urn:RcdManager:management"
	DefaultMonitorBatchSize = 500
	// The current Rust lower caps an OPC Session timeout at 60 seconds and does
	// not treat Publish traffic as Session activity. Probe well inside that cap.
	DefaultSessionProbeInterval = 20 * time.Second
)

var ErrLowerUnavailable = errors.New("lower OPC source is unavailable")

type ConnectionState string

const (
	ConnectionDisconnected ConnectionState = "Disconnected"
	ConnectionConnecting   ConnectionState = "Connecting"
	ConnectionConnected    ConnectionState = "Connected"
)

type SessionState string

const (
	SessionInactive SessionState = "Inactive"
	SessionActive   SessionState = "Active"
)

type SubscriptionState string

const (
	SubscriptionInactive SubscriptionState = "Inactive"
	SubscriptionCreating SubscriptionState = "Creating"
	SubscriptionActive   SubscriptionState = "Active"
)

// LowerStatus is an immutable status snapshot for one downstream source.
type LowerStatus struct {
	SourceID          string
	Connection        ConnectionState
	Session           SessionState
	Subscription      SubscriptionState
	Catalog           domain.CatalogState
	Epoch             uint64
	ReconnectCount    uint64
	PointCount        int
	SubscriptionCount int
	LastDataAt        time.Time
	LastProbeAt       time.Time
	LastError         string
	UpdatedAt         time.Time
}

type LowerProbeStage struct {
	Name       string
	OK         bool
	Message    string
	DurationMS int64
}

type LowerProbeResult struct {
	EndpointCount  int
	NamespaceCount int
	VariableCount  int
	MethodCount    int
	DurationMS     int64
	Stages         []LowerProbeStage
	Error          string
	TestedAt       time.Time
}

type PointStatistics struct {
	Good  int
	Bad   int
	Total int
}

type LowerSink interface {
	OnLowerStatus(LowerStatus)
	OnCatalog(domain.SourceCatalog)
	OnSample(sourceID string, key domain.NodeKey, sample domain.Sample)
}

type NopLowerSink struct{}

func (NopLowerSink) OnLowerStatus(LowerStatus)                      {}
func (NopLowerSink) OnCatalog(domain.SourceCatalog)                 {}
func (NopLowerSink) OnSample(string, domain.NodeKey, domain.Sample) {}

type LowerConfig struct {
	Source               domain.Source
	Password             string
	CertificateFile      string
	PrivateKeyFile       string
	PublishingInterval   time.Duration
	SamplingInterval     time.Duration
	MonitorBatchSize     int
	ConnectTimeout       time.Duration
	OperationTimeout     time.Duration
	SessionProbeInterval time.Duration
	MinReconnectBackoff  time.Duration
	MaxReconnectBackoff  time.Duration
}

type LowerClient interface {
	Run(context.Context) error
	Read(context.Context, domain.NodeKey) (domain.Sample, error)
	Write(context.Context, domain.NodeKey, any) (domain.StatusCode, error)
	Call(context.Context, domain.NodeKey, string) (string, domain.StatusCode, error)
	Status() LowerStatus
}

// EpochPinnedLowerClient is implemented by supervisors that can bind a command
// to the exact downstream connection epoch captured when routing began.
type EpochPinnedLowerClient interface {
	WriteAtEpoch(context.Context, domain.NodeKey, any, uint64) (domain.StatusCode, error)
	CallAtEpoch(context.Context, domain.NodeKey, string, uint64) (string, domain.StatusCode, error)
}

// UpstreamBackend is implemented by the manager runtime. Command methods must
// pin the selected SourceID before making a lower OPC request.
type UpstreamBackend interface {
	OpenSession(sessionID string) error
	CloseSession(sessionID string)
	TouchSession(sessionID string) error
	ReadSample(sessionID string, key domain.NodeKey) domain.Sample
	WriteCommand(ctx context.Context, sessionID string, key domain.NodeKey, value any) (domain.StatusCode, error)
	CallCommand(ctx context.Context, sessionID string, key domain.NodeKey, hexData string) (string, domain.StatusCode, error)
	SelectSource(sessionID, groupID, sourceID string, expectedRevision uint64) (domain.GroupRoute, error)
	SetDefaultSource(groupID, sourceID string, expectedRevision uint64) (domain.Group, error)
	Session(sessionID string) (domain.Session, error)
	Groups() []domain.Group
	Sources() []domain.Source
	LowerStatus(sourceID string) (LowerStatus, bool)
	SourcePointStatistics(sourceID string) PointStatistics
	GroupForNode(identifier string) (string, bool)
	PendingCount(groupID string) int
}

// GroupCommandBackend exposes unambiguous logical-device commands without
// changing the legacy physical-port NodeIds. Implementations must retain the
// same per-source physical-port lock used by the legacy command handlers.
type GroupCommandBackend interface {
	ReadGroupCommand(sessionID, groupID string) domain.Sample
	WriteGroupCommand(ctx context.Context, sessionID, groupID string, value any) (domain.StatusCode, error)
	CallGroupCommand(ctx context.Context, sessionID, groupID, hexData string) (string, domain.StatusCode, error)
}
