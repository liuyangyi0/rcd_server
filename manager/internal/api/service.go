package api

import (
	"context"
	"time"

	"rcd_manager/internal/domain"
)

type Overview struct {
	ManagerState         string    `json:"manager_state"`
	OPCEndpoint          string    `json:"opc_endpoint"`
	GroupCount           int       `json:"group_count"`
	SourceCount          int       `json:"source_count"`
	ConnectedSources     int       `json:"connected_sources"`
	SessionCount         int       `json:"session_count"`
	SubscriptionCount    int       `json:"subscription_count"`
	PointCount           int       `json:"point_count"`
	GoodPoints           int       `json:"good_points"`
	BadPoints            int       `json:"bad_points"`
	PendingCommands      int       `json:"pending_commands"`
	CatalogConflicts     int       `json:"catalog_conflicts"`
	OpenAlarms           int       `json:"open_alarms"`
	UnacknowledgedAlarms int       `json:"unacknowledged_alarms"`
	RecentSelectionAt    time.Time `json:"recent_selection_at,omitempty,omitzero"`
	RecentSelectionCount uint64    `json:"recent_selection_count"`
	Version              string    `json:"version,omitempty"`
	GitSHA               string    `json:"git_sha,omitempty"`
	SystemTime           time.Time `json:"system_time"`
	StartedAt            time.Time `json:"started_at"`
	LastLowerDataAt      time.Time `json:"last_lower_data_at,omitempty,omitzero"`
}

type GroupView struct {
	domain.Group
	SelectedSourceID  string    `json:"selected_source_id,omitempty"`
	SelectionRevision uint64    `json:"selection_revision,omitempty"`
	MemberSourceIDs   []string  `json:"member_source_ids"`
	SourceCount       int       `json:"source_count"`
	GoodPoints        int       `json:"good_points"`
	BadPoints         int       `json:"bad_points"`
	TotalPoints       int       `json:"total_points"`
	PendingCommands   int       `json:"pending_commands"`
	LastSelectedAt    time.Time `json:"last_selected_at,omitempty,omitzero"`
	Status            string    `json:"status"`
}

type SourceView struct {
	domain.Source
	CatalogConflicts  []domain.CatalogConflictDetail `json:"catalog_conflicts,omitempty"`
	ConnectionState   string                         `json:"connection_state"`
	SessionState      string                         `json:"session_state"`
	SubscriptionState string                         `json:"subscription_state"`
	LastDataAt        time.Time                      `json:"last_data_at,omitempty,omitzero"`
	LastProbeAt       time.Time                      `json:"last_probe_at,omitempty,omitzero"`
	LastError         string                         `json:"last_error,omitempty"`
	ReconnectCount    uint64                         `json:"reconnect_count"`
	MonitoredCount    int                            `json:"monitored_count"`
	GoodPoints        int                            `json:"good_points"`
	BadPoints         int                            `json:"bad_points"`
	TotalPoints       int                            `json:"total_points"`
}

type SessionView struct {
	domain.Session
	ClientName           string `json:"client_name,omitempty"`
	ClientApplicationURI string `json:"client_application_uri,omitempty"`
	RemoteAddress        string `json:"remote_address,omitempty"`
}

type PointView struct {
	GroupID          string             `json:"group_id"`
	SourceIDs        []string           `json:"source_ids"`
	Node             domain.CatalogNode `json:"node"`
	SelectedSourceID string             `json:"selected_source_id,omitempty"`
	SourcePresent    *bool              `json:"source_present,omitempty"`
	Sample           *domain.Sample     `json:"sample,omitempty"`
}

type PageRequest struct {
	PortID        string
	DeviceID      string
	Limit         int
	Offset        int
	GroupID       string
	SourceID      string
	RouteSourceID string
	SessionID     string
	Status        string
	Kind          string
	Query         string
	From          time.Time
	To            time.Time
}

type PointPage struct {
	Items  []PointView `json:"items"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

type CommandPage struct {
	Items  []domain.PendingCommand `json:"items"`
	Total  int                     `json:"total"`
	Limit  int                     `json:"limit"`
	Offset int                     `json:"offset"`
}

type AlarmPage struct {
	Items  []domain.Alarm `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type SourceEventPage struct {
	Items  []domain.SourceEvent `json:"items"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

type AuditPage struct {
	Items  []domain.AuditEntry `json:"items"`
	Total  int                 `json:"total"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
}

type SourceProbeStage struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type SourceProbeResult struct {
	SourceID       string             `json:"source_id"`
	Endpoint       string             `json:"endpoint"`
	OK             bool               `json:"ok"`
	Error          string             `json:"error,omitempty"`
	EndpointCount  int                `json:"endpoint_count"`
	NamespaceCount int                `json:"namespace_count"`
	VariableCount  int                `json:"variable_count"`
	MethodCount    int                `json:"method_count"`
	DurationMS     int64              `json:"duration_ms"`
	Stages         []SourceProbeStage `json:"stages"`
	TestedAt       time.Time          `json:"tested_at"`
}

type CertificateInfo struct {
	Error           string    `json:"error,omitempty"`
	Subject         string    `json:"subject"`
	SerialNumber    string    `json:"serial_number"`
	SHA256          string    `json:"sha256"`
	NotBefore       time.Time `json:"not_before"`
	NotAfter        time.Time `json:"not_after"`
	DaysRemaining   int       `json:"days_remaining"`
	ValidNow        bool      `json:"valid_now"`
	ExpiryWarning   bool      `json:"expiry_warning"`
	ApplicationURIs []string  `json:"application_uris"`
}

type StorageInfo struct {
	DataDirectoryBytes int64 `json:"data_directory_bytes"`
	DatabaseBytes      int64 `json:"database_bytes"`
	LogBytes           int64 `json:"log_bytes"`
	DiskFreeBytes      int64 `json:"disk_free_bytes"`
	AuditRetentionDays int   `json:"audit_retention_days"`
	MaxAuditRows       int   `json:"max_audit_rows"`
}

type RuntimeMetrics struct {
	Goroutines                int    `json:"goroutines"`
	AllocatedBytes            uint64 `json:"allocated_bytes"`
	HeapObjects               uint64 `json:"heap_objects"`
	RefreshQueueDepth         int    `json:"refresh_queue_depth"`
	DirtyCatalogs             int    `json:"dirty_catalogs"`
	DirtyCommands             int    `json:"dirty_commands"`
	DirtyAudits               int    `json:"dirty_audits"`
	DirtyStatusEvents         int    `json:"dirty_status_events"`
	DirtyStatusAlarms         int    `json:"dirty_status_alarms"`
	EventSubscribers          int    `json:"event_subscribers"`
	DroppedEventNotifications uint64 `json:"dropped_event_notifications"`
}

type SystemInfo struct {
	OPCEndpoint          string          `json:"opc_endpoint"`
	HTTPListen           string          `json:"http_listen"`
	DataDirectory        string          `json:"data_directory"`
	DatabasePath         string          `json:"database_path"`
	LogDirectory         string          `json:"log_directory"`
	CommandTimeoutMS     int64           `json:"command_timeout_ms"`
	PendingTimeoutMS     int64           `json:"pending_timeout_ms"`
	SamplingIntervalMS   int64           `json:"sampling_interval_ms"`
	PublishingIntervalMS int64           `json:"publishing_interval_ms"`
	MaxSources           int             `json:"max_sources"`
	MaxSessions          int             `json:"max_sessions"`
	Certificate          CertificateInfo `json:"certificate"`
	Storage              StorageInfo     `json:"storage"`
	Metrics              RuntimeMetrics  `json:"metrics"`
}

type ExportSource struct {
	ID             string                `json:"id"`
	GroupID        string                `json:"group_id"`
	GroupBindings  []domain.GroupBinding `json:"group_bindings,omitempty"`
	Name           string                `json:"name"`
	Endpoint       string                `json:"endpoint"`
	SecurityPolicy string                `json:"security_policy"`
	SecurityMode   string                `json:"security_mode"`
	Username       string                `json:"username,omitempty"`
	Enabled        bool                  `json:"enabled"`
}

type ConfigurationDocument struct {
	SchemaVersion int             `json:"schema_version"`
	ExportedAt    time.Time       `json:"exported_at,omitempty,omitzero"`
	Groups        []GroupMutation `json:"groups"`
	Sources       []ExportSource  `json:"sources"`
}

type ConfigurationImportRequest struct {
	Document ConfigurationDocument `json:"document"`
	DryRun   bool                  `json:"dry_run"`
}

type ConfigurationImportResult struct {
	DryRun         bool     `json:"dry_run"`
	CreatedGroups  int      `json:"created_groups"`
	UpdatedGroups  int      `json:"updated_groups"`
	CreatedSources int      `json:"created_sources"`
	UpdatedSources int      `json:"updated_sources"`
	Warnings       []string `json:"warnings"`
}

type GroupMutation struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	DefaultSourceID string `json:"default_source_id,omitempty"`
}

type SourceMutation struct {
	ID             string                `json:"id,omitempty"`
	GroupID        string                `json:"group_id"`
	GroupBindings  []domain.GroupBinding `json:"group_bindings"`
	Name           string                `json:"name"`
	Endpoint       string                `json:"endpoint"`
	SecurityPolicy string                `json:"security_policy"`
	SecurityMode   string                `json:"security_mode"`
	Username       string                `json:"username,omitempty"`
	Password       *string               `json:"password,omitempty"`
	Enabled        bool                  `json:"enabled"`
}

type HealthStatus struct {
	OK     bool              `json:"ok"`
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
	Time   time.Time         `json:"time"`
}

type Event struct {
	ID        string    `json:"id,omitempty"`
	Type      string    `json:"type"`
	GroupID   string    `json:"group_id,omitempty"`
	SourceID  string    `json:"source_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Revision  uint64    `json:"revision,omitempty"`
	Data      any       `json:"data,omitempty"`
	Time      time.Time `json:"occurred_at"`
}

type Subscription struct {
	Events <-chan Event
	Close  func()
}

// Service is the complete application boundary used by HTTP. Implementations
// combine the persistent repository with live OPC state; handlers never invent
// connection state or point values from cached catalog rows.
type Service interface {
	Overview(context.Context) (Overview, error)

	ListGroups(context.Context) ([]GroupView, error)
	GetGroup(context.Context, string) (GroupView, error)
	CreateGroup(context.Context, GroupMutation) (GroupView, error)
	UpdateGroup(context.Context, string, uint64, GroupMutation) (GroupView, error)
	DeleteGroup(context.Context, string, uint64) error
	SetDefaultSource(context.Context, string, string, uint64) (GroupView, error)

	ListSources(context.Context) ([]SourceView, error)
	GetSource(context.Context, string) (SourceView, error)
	CreateSource(context.Context, SourceMutation) (SourceView, error)
	UpdateSource(context.Context, string, uint64, SourceMutation) (SourceView, error)
	DeleteSource(context.Context, string, uint64) error
	ProbeSource(context.Context, string) (SourceProbeResult, error)
	RestartSource(context.Context, string, uint64, bool) (SourceView, error)

	ListSessions(context.Context) ([]SessionView, error)
	SetSessionSelection(context.Context, string, string, string, uint64) (domain.GroupRoute, error)

	ListPoints(context.Context, PageRequest) (PointPage, error)
	ListCommands(context.Context, PageRequest) (CommandPage, error)
	ListAlarms(context.Context, PageRequest) (AlarmPage, error)
	AcknowledgeAlarm(context.Context, int64, uint64) (domain.Alarm, error)
	SilenceAlarm(context.Context, int64, uint64, time.Time) (domain.Alarm, error)
	ListSourceEvents(context.Context, PageRequest) (SourceEventPage, error)
	ListManagementAudit(context.Context, PageRequest) (AuditPage, error)
	SystemInfo(context.Context) (SystemInfo, error)
	ExportConfiguration(context.Context) (ConfigurationDocument, error)
	ImportConfiguration(context.Context, ConfigurationImportRequest) (ConfigurationImportResult, error)
	DiagnosticBundle(context.Context) ([]byte, error)
	PrometheusMetrics(context.Context) (string, error)
	Subscribe(context.Context) (Subscription, error)

	Health(context.Context) HealthStatus
	Ready(context.Context) HealthStatus
}
