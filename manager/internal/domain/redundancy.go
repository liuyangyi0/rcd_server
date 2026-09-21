package domain

import (
	"fmt"
	"math"
	"reflect"
	"time"
)

// Policy is opt-in. Priority lists are ordered physical source IDs, not names/IPs.
type RedundancyPolicy struct {
	GroupID           string   `json:"group_id"`
	Revision          uint64   `json:"revision"`
	Enabled           bool     `json:"enabled"`
	DefaultAutomatic  bool     `json:"default_automatic"`
	Priority          []string `json:"priority"`
	FailureSeconds    int      `json:"failure_seconds"`
	HealthySeconds    int      `json:"healthy_seconds"`
	CooldownSeconds   int      `json:"cooldown_seconds"`
	CompareEnabled    bool     `json:"compare_enabled"`
	LeftSourceID      string   `json:"left_source_id"`
	RightSourceID     string   `json:"right_source_id"`
	DifferenceSeconds int      `json:"difference_seconds"`
	AbsoluteTolerance float64  `json:"absolute_tolerance"`
	RelativeTolerance float64  `json:"relative_tolerance"`
	MaxSkewMS         int      `json:"max_skew_ms"`
}

func DefaultRedundancyPolicy(group string) RedundancyPolicy {
	return RedundancyPolicy{GroupID: group, Revision: 1, Priority: []string{}, FailureSeconds: 5, HealthySeconds: 5, CooldownSeconds: 30, DifferenceSeconds: 5, MaxSkewMS: 2000}
}
func (p RedundancyPolicy) Clone() RedundancyPolicy {
	p.Priority = append([]string{}, p.Priority...)
	return p
}
func (p RedundancyPolicy) Validate() error {
	if p.FailureSeconds < 1 || p.FailureSeconds > 300 || p.HealthySeconds < 1 || p.HealthySeconds > 300 || p.CooldownSeconds < 1 || p.CooldownSeconds > 3600 || p.DifferenceSeconds < 1 || p.DifferenceSeconds > 300 || p.MaxSkewMS < 1 || p.MaxSkewMS > 60000 {
		return &FieldError{Field: "redundancy", Message: "invalid delay: failure/healthy/difference 1..300 s, cooldown 1..3600 s, skew 1..60000 ms"}
	}
	if math.IsNaN(p.AbsoluteTolerance) || math.IsInf(p.AbsoluteTolerance, 0) || p.AbsoluteTolerance < 0 || math.IsNaN(p.RelativeTolerance) || math.IsInf(p.RelativeTolerance, 0) || p.RelativeTolerance < 0 || p.RelativeTolerance > 1 {
		return &FieldError{Field: "tolerance", Message: "absolute tolerance must be finite and nonnegative; relative tolerance must be 0..1"}
	}
	seen := map[string]bool{}
	for _, id := range p.Priority {
		if id == "" || seen[id] {
			return &FieldError{Field: "priority", Message: "source IDs must be nonempty and unique"}
		}
		seen[id] = true
	}
	if len(p.Priority) > 20 || (p.Enabled && len(p.Priority) < 2) {
		return &FieldError{Field: "priority", Message: "automatic switching requires 2..20 ordered sources"}
	}
	if p.CompareEnabled && (p.LeftSourceID == "" || p.RightSourceID == "" || p.LeftSourceID == p.RightSourceID) {
		return &FieldError{Field: "comparison", Message: "select two distinct sources"}
	}
	return nil
}

type SourceHealth struct {
	SourceID string    `json:"source_id"`
	Healthy  bool      `json:"healthy"`
	Reason   string    `json:"reason"`
	Since    time.Time `json:"since"`
}
type PointDifference struct {
	NodeID string    `json:"node_id"`
	Left   any       `json:"left"`
	Right  any       `json:"right"`
	Since  time.Time `json:"since"`
}
type RedundancyView struct {
	Policy                RedundancyPolicy      `json:"policy"`
	Health                []SourceHealth        `json:"health"`
	Routes                map[string]GroupRoute `json:"routes"`
	LastSwitchAt          time.Time             `json:"last_switch_at,omitempty,omitzero"`
	LastSwitchReason      string                `json:"last_switch_reason"`
	ComparisonState       string                `json:"comparison_state"`
	ComparedCount         int                   `json:"compared_count"`
	SkippedCount          int                   `json:"skipped_count"`
	DifferenceCount       int                   `json:"difference_count"`
	DifferenceAlarmActive bool                  `json:"difference_alarm_active"`
	Differences           []PointDifference     `json:"differences"`
	EvaluatedAt           time.Time             `json:"evaluated_at,omitempty,omitzero"`
	LastError             string                `json:"last_error"`
}

// Compare only current, good scalar samples with usable, sufficiently aligned
// source times. Unavailable is different from equal. Static values need not change.
func CompareSamples(a, b Sample, p RedundancyPolicy) (different, comparable bool) {
	if !a.StatusCode.IsGood() || !b.StatusCode.IsGood() || a.Type != b.Type || a.SourceTimestamp.IsZero() || b.SourceTimestamp.IsZero() {
		return false, false
	}
	skew := a.SourceTimestamp.Sub(b.SourceTimestamp)
	if skew < 0 {
		skew = -skew
	}
	if skew > time.Duration(p.MaxSkewMS)*time.Millisecond {
		return false, false
	}
	switch a.Type {
	case DataTypeDouble, DataTypeFloat, DataTypeUInt32:
		x, ok := number(a.Value)
		y, ok2 := number(b.Value)
		if !ok || !ok2 || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
			return false, false
		}
		return math.Abs(x-y) > math.Max(p.AbsoluteTolerance, p.RelativeTolerance*math.Max(math.Abs(x), math.Abs(y))), true
	case DataTypeBoolean, DataTypeString:
		return !reflect.DeepEqual(a.Value, b.Value), true
	default:
		return false, false
	}
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case uint32:
		return float64(n), true
	}
	return 0, false
}
func (p RedundancyPolicy) DifferenceKind() string {
	return fmt.Sprintf("group/%s/data_difference", p.GroupID)
}
