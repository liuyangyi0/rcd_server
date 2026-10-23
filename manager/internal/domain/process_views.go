package domain

// Transport-neutral process requests and snapshots; API aliases preserve the wire contract.
type ProcessActionInput struct {
	Action                  string            `json:"action"`
	RequestID               string            `json:"request_id"`
	SourceRevision          uint64            `json:"source_revision"`
	BindingRevision         uint64            `json:"binding_revision"`
	ExpectedConfigRevisions map[string]string `json:"expected_config_revisions,omitempty"`
}

type ProcessStatusView struct {
	ProcessSnapshot
	SourceID         string            `json:"source_id"`
	SourceRevision   uint64            `json:"source_revision"`
	BindingRevision  uint64            `json:"binding_revision"`
	PendingOperation *ProcessOperation `json:"pending_operation,omitempty"`
}

type ProcessLogView struct {
	ProcessLog
	SourceID        string `json:"source_id"`
	SourceRevision  uint64 `json:"source_revision"`
	BindingRevision uint64 `json:"binding_revision"`
}

type ProcessOperationPage struct {
	SourceID       string             `json:"source_id"`
	SourceRevision uint64             `json:"source_revision"`
	Items          []ProcessOperation `json:"items"`
	Total          int                `json:"total"`
	Limit          int                `json:"limit"`
	Offset         int                `json:"offset"`
}
