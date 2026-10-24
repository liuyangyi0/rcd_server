package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// These DTOs are the entire anonymous surface. Never embed Source, Session,
// configuration, command payloads, or unfiltered diagnostic/error objects here.
type FlowSnapshot struct {
	GeneratedAt     time.Time       `json:"generated_at"`
	Instance        string          `json:"instance"`
	Endpoint        string          `json:"endpoint"`
	Running         bool            `json:"running"`
	PendingCommands int             `json:"pending_commands"`
	Sources         []FlowSource    `json:"sources"`
	Groups          []FlowGroup     `json:"groups"`
	Sessions        []FlowSession   `json:"sessions"`
	Calculation     FlowCalculation `json:"calculation"`
}

type FlowSource struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Address      string     `json:"address"`
	Enabled      bool       `json:"enabled"`
	Connected    bool       `json:"connected"`
	Subscribed   bool       `json:"subscribed"`
	CatalogState string     `json:"catalog_state"`
	LastDataAt   time.Time  `json:"last_data_at,omitempty,omitzero"`
	Samples      string     `json:"samples"` // decimal uint64, lossless in JavaScript
	Ports        []FlowPort `json:"ports"`
	Truncated    bool       `json:"truncated"`
}

type FlowPort struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Devices []FlowDevice `json:"devices"`
}

type FlowDevice struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	GroupID    string    `json:"group_id"`
	State      uint32    `json:"state"` // existing CommunicationState 0..5 contract
	ObservedAt time.Time `json:"observed_at,omitempty,omitzero"`
	PointCount int       `json:"point_count"`
}

type FlowGroup struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	DefaultSource string   `json:"default_source"`
	Members       []string `json:"members"`
}

type FlowSession struct {
	ID             string            `json:"id"` // one-way public alias, never the OPC Session NodeId
	Name           string            `json:"name"`
	Attached       bool              `json:"attached"`
	Routes         map[string]string `json:"routes"`
	Subscriptions  int               `json:"subscriptions"`
	MonitoredItems uint32            `json:"monitored_items"`
	Notifications  map[string]uint32 `json:"notifications"` // generated notifications, not delivery ACKs
}

type FlowCalculation struct {
	Active           bool      `json:"active"`
	Rules            int       `json:"rules"`
	Good             int       `json:"good"`
	Bad              int       `json:"bad"`
	LastEvaluationAt time.Time `json:"last_evaluation_at,omitempty,omitzero"`
	DurationMS       float64   `json:"duration_ms"`
}

type FlowPoint struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Value           json.RawMessage `json:"value"`
	Status          uint32          `json:"status"`
	SourceTimestamp time.Time       `json:"source_timestamp,omitempty,omitzero"`
	ValueOmitted    bool            `json:"value_omitted"`
}

type FlowPointPage struct {
	Items  []FlowPoint `json:"items"`
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
}

type PublicFlowService interface {
	PublicFlow(context.Context) (FlowSnapshot, error)
	PublicFlowPoints(context.Context, PageRequest) (FlowPointPage, error)
}

// Exact read-only allowlist. No prefix bypass, including unknown future routes.
func isPublicFlowRead(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.URL.Path == "/api/v1/public/flow" || r.URL.Path == "/api/v1/public/flow/points")
}

func (h *Handler) publicFlow(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(PublicFlowService)
	if !ok {
		writeError(w, 503, "flow_unavailable", "数据流监视暂不可用", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	payload, err := h.publicFlowPayload(ctx, service)
	if err != nil {
		writeError(w, 503, "flow_unavailable", "读取数据流状态失败", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}

// All viewers share one immutable snapshot per second. Network writes happen
// after releasing this lock so a slow display cannot stall other viewers.
func (h *Handler) publicFlowPayload(ctx context.Context, service PublicFlowService) ([]byte, error) {
	h.flowMu.Lock()
	defer h.flowMu.Unlock()
	if len(h.flowJSON) == 0 || h.now().Sub(h.flowCachedAt) >= time.Second {
		value, err := service.PublicFlow(ctx)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		h.flowJSON, h.flowCachedAt = payload, h.now()
	}
	return h.flowJSON, nil
}

func (h *Handler) publicFlowPoints(w http.ResponseWriter, r *http.Request) {
	service, ok := h.service.(PublicFlowService)
	if !ok {
		writeError(w, 503, "flow_unavailable", "数据流监视暂不可用", nil)
		return
	}
	q := r.URL.Query()
	if len(q.Get("source_id")) == 0 || len(q.Get("source_id")) > 128 || len(q.Get("q")) > 128 || len(q.Get("port_id")) > 256 || len(q.Get("device_id")) > 128 {
		writeError(w, 400, "invalid_filter", "请选择有效来源，筛选内容过长", nil)
		return
	}
	offset := 0
	if raw := q.Get("offset"); raw != "" {
		var err error
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 || offset > 100000 {
			writeError(w, 400, "invalid_offset", "点位页码无效", nil)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	value, err := service.PublicFlowPoints(ctx, PageRequest{SourceID: q.Get("source_id"), PortID: q.Get("port_id"), DeviceID: q.Get("device_id"), Query: q.Get("q"), Limit: 20, Offset: offset})
	if err != nil {
		writeError(w, 404, "flow_source_missing", "来源不存在或目录尚未就绪", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, value)
}
