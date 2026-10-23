package calculation

import "time"

type Status struct {
	RoutingMode              string    `json:"routing_mode"`
	ActiveRevision           uint64    `json:"active_revision"`
	ConfigHash               string    `json:"config_hash,omitempty"`
	RuleCount                int       `json:"rule_count"`
	GoodCount                int       `json:"good_count"`
	BadCount                 int       `json:"bad_count"`
	DefaultRouteRevision     uint64    `json:"default_route_revision"`
	LastEvaluationAt         time.Time `json:"last_evaluation_at,omitempty,omitzero"`
	LastEvaluationDurationMS float64   `json:"last_evaluation_duration_ms"`
	LastError                string    `json:"last_error,omitempty"`
	PendingRestart           bool      `json:"pending_restart"`
	PendingRevision          uint64    `json:"pending_revision,omitempty"`
	PendingHash              string    `json:"pending_hash,omitempty"`
}
