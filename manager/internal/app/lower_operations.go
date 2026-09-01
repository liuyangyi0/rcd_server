package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

const lowerOperationsTimeout = 15 * time.Second

type lowerOperationsTarget struct {
	source              domain.Source
	client              opc.LowerOperationsClient
	epoch               uint64
	connectionEpoch     uint64
	topologyFingerprint string
}

// configMu must be held. Never retain it during an OPC round trip: a slow
// maintenance request must not block Session creation, routing, or hot data.
func (r *Runtime) lowerOperationsTargetLocked(id string) (lowerOperationsTarget, error) {
	if r.closing.Load() {
		return lowerOperationsTarget{}, domain.ErrShuttingDown
	}
	source, err := r.source(id)
	if err != nil {
		return lowerOperationsTarget{}, err
	}
	target := lowerOperationsTarget{source: source}
	if !source.Enabled {
		return target, unavailableLowerOperation()
	}
	r.mu.RLock()
	handle, exists := r.lowers[id]
	status := r.statuses[id]
	topologyFingerprint := domain.SourceTopologyFingerprint(r.catalogs[id].Topology)
	r.mu.RUnlock()
	if !exists || status.Connection != opc.ConnectionConnected || status.Session != opc.SessionActive {
		return target, unavailableLowerOperation()
	}
	client, ok := handle.client.(opc.LowerOperationsClient)
	if !ok {
		return target, &domain.OperationError{Code: "unsupported", Message: "该下级尚未提供运维能力"}
	}
	target.client, target.epoch, target.connectionEpoch = client, status.Epoch, status.Epoch
	if topologyFingerprint == "" {
		topologyFingerprint = unverifiedStationTopology
	}
	target.topologyFingerprint = topologyFingerprint
	if status.Epoch>>32 == handle.generation {
		target.epoch = status.Epoch & 0xffffffff
	}
	return target, nil
}

func (r *Runtime) SourceOperationsCapabilities(ctx context.Context, id string) (domain.OperationsCapabilities, error) {
	r.configMu.RLock()
	target, err := r.lowerOperationsTargetLocked(id)
	r.configMu.RUnlock()
	var operationErr *domain.OperationError
	if errors.As(err, &operationErr) && operationErr.Code == "unsupported" {
		return domain.OperationsCapabilities{SourceID: id, SourceRevision: target.source.Revision, Reason: operationErr.Message, Features: []string{}}, nil
	}
	if err != nil {
		return domain.OperationsCapabilities{}, err
	}
	operationContext, cancel := context.WithTimeout(ctx, lowerOperationsTimeout)
	defer cancel()
	capabilities, err := target.client.OperationsCapabilities(operationContext, target.epoch)
	if err != nil {
		return domain.OperationsCapabilities{}, normalizeLowerOperationError(operationContext, err, false)
	}
	capabilities.SourceID, capabilities.SourceRevision = id, target.source.Revision
	return capabilities, nil
}

func (r *Runtime) ExecuteSourceOperation(ctx context.Context, id string, sourceRevision uint64, request domain.OperationRequest) (json.RawMessage, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	r.configMu.Lock()
	var target lowerOperationsTarget
	var err error
	if request.Mutates() {
		// A process restart deliberately makes OPC unavailable. Check the
		// persistent maintenance fence before availability so a concurrent
		// mutation consistently reports conflict, not a misleading outage.
		if r.closing.Load() {
			err = domain.ErrShuttingDown
		} else {
			var source domain.Source
			source, err = r.source(id)
			if err == nil && sourceRevision != source.Revision {
				err = &domain.RevisionConflictError{Expected: sourceRevision, Actual: source.Revision}
			}
		}
		if err == nil {
			err = r.checkSourceOperationLocked(id)
		}
	}
	if err == nil {
		target, err = r.lowerOperationsTargetLocked(id)
	}
	if err == nil && request.Mutates() {
		if request.Feature() == "acquisition" || request.Feature() == "station_versions" {
			if r.physicalPortCommandPending(id, request.PortID) {
				err = domain.ErrCommandPending
			}
		}
		if err == nil {
			if r.sourceOperations == nil {
				r.sourceOperations = make(map[string]bool)
			}
			r.sourceOperations[id] = true
		}
	}
	r.configMu.Unlock()
	if err != nil {
		return nil, err
	}
	if request.Mutates() {
		defer func() {
			r.configMu.Lock()
			delete(r.sourceOperations, id)
			r.configMu.Unlock()
		}()
	}
	operationContext, cancel := context.WithTimeout(ctx, lowerOperationsTimeout)
	defer cancel()
	capabilities, err := target.client.OperationsCapabilities(operationContext, target.epoch)
	if err != nil {
		return nil, normalizeLowerOperationError(operationContext, err, false)
	}
	if !capabilities.Supported || !capabilities.HasFeature(request.Feature()) {
		return nil, &domain.OperationError{Code: "unsupported", Message: "该下级未提供此项运维能力"}
	}

	started := time.Now()
	var auditDetails map[string]any
	if request.Mutates() {
		auditDetails = operationAuditDetails(request)
		r.audit(ctx, "source.operation.requested", id, target.source.Revision, auditDetails)
	}
	// Exactly one dispatch, bound to the captured client and connection epoch.
	// A timeout is an uncertain outcome, not an invitation to retry the write.
	result, err := target.client.ExecuteOperation(operationContext, request, target.epoch)
	err = normalizeLowerOperationError(operationContext, err, request.Mutates())
	if err == nil {
		result, err = bindOperationSource(result, target.source)
		if err == nil {
			if cacheErr := r.cacheStationVersionResult(request, result, target.source, target.connectionEpoch, target.topologyFingerprint); cacheErr != nil {
				slog.Error("cache station versions", "source_id", id, "error", cacheErr)
			}
		}
	}
	if request.Mutates() {
		completed := make(map[string]any, len(auditDetails)+3)
		for key, value := range auditDetails {
			completed[key] = value
		}
		completed["duration_ms"] = time.Since(started).Milliseconds()
		completed["outcome"] = "success"
		if err != nil {
			completed["outcome"] = "failed"
			var operationErr *domain.OperationError
			if errors.As(err, &operationErr) {
				completed["error_code"] = operationErr.Code
				if operationErr.Code == "timeout" || operationErr.Code == "unavailable" || operationErr.Code == "invalid_response" {
					completed["outcome"] = "unknown"
				}
			}
		} else {
			var status struct {
				Status                   string `json:"status"`
				Revision                 string `json:"revision"`
				OperationRevision        string `json:"operation_revision"`
				StationOperationRevision string `json:"station_operation_revision"`
			}
			if json.Unmarshal(result, &status) == nil {
				completed["lower_revision"] = status.Revision
				if status.OperationRevision != "" {
					// Other physical ports may advance the lower snapshot while
					// this operation waits for its worker receipt. Correlate this
					// audit to its own commit, not that later snapshot.
					completed["lower_revision"] = status.OperationRevision
					completed["operation_revision"] = status.OperationRevision
					completed["snapshot_revision"] = status.Revision
				}
				if status.Status == "Pending" {
					completed["outcome"] = "pending"
				}
				if status.StationOperationRevision != "" {
					completed["station_operation_revision"] = status.StationOperationRevision
				}
			}
			if request.Feature() == "station_versions" {
				for key, value := range stationOperationAuditResult(result, request) {
					completed[key] = value
				}
			}
		}
		r.audit(ctx, "source.operation.completed", id, target.source.Revision, completed)
		r.broker.Publish(api.Event{Type: "source.operation", SourceID: id, Data: map[string]any{"source_id": id, "action": request.Action}})
	}
	return result, err
}

func (r *Runtime) checkSourceOperationLocked(id string) error {
	if err := r.checkProcessMaintenanceLocked(id); err != nil {
		return err
	}
	if r.sourceOperations[id] {
		return &domain.OperationError{Code: "busy", Message: "该下级有运维修改正在执行，请刷新状态后重试"}
	}
	return nil
}

func (r *Runtime) physicalPortCommandPending(sourceID, portID string) bool {
	// Operations use the raw physical identifier; legacy command locks use the
	// normalized OPC port stem. Resolve from the same discovered topology.
	port := portID
	r.mu.RLock()
	if topology := r.catalogs[sourceID].Topology; topology != nil {
		for _, item := range topology.Ports {
			if item.PortID == portID {
				port = strings.TrimSuffix(item.SendNodeID, "_Send")
				break
			}
		}
	}
	r.mu.RUnlock()
	_, pending := r.routes.PendingCommand(sourceID, port)
	return pending
}

func bindOperationSource(result json.RawMessage, source domain.Source) (json.RawMessage, error) {
	if len(result) > domain.MaxOperationJSONBytes {
		return nil, &domain.OperationError{Code: "invalid_response", Message: "下级运维结果超限"}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil || fields == nil {
		return nil, &domain.OperationError{Code: "invalid_response", Message: "下级运维结果格式无效"}
	}
	fields["source_id"], _ = json.Marshal(source.ID)
	fields["source_revision"], _ = json.Marshal(source.Revision)
	return json.Marshal(fields)
}

func operationAuditDetails(request domain.OperationRequest) map[string]any {
	identifier := make([]byte, 16)
	if _, err := rand.Read(identifier); err != nil {
		identifier = []byte(fmt.Sprint(time.Now().UnixNano()))
	}
	details := map[string]any{"operation_id": hex.EncodeToString(identifier), "action": request.Action, "expected_lower_revision": request.ExpectedRevision}
	if request.PortID != "" {
		details["port_id"] = request.PortID
	}
	if request.DeviceID != "" {
		details["device_id"] = request.DeviceID
	}
	if request.Enabled != nil {
		details["enabled"] = *request.Enabled
	}
	if request.ModuleType != "" {
		details["module_type"] = request.ModuleType
	}
	if request.Mode != "" {
		details["mode"] = request.Mode
	}
	if request.LogLevel != "" {
		details["log_level"] = request.LogLevel
	}
	if request.ExpectedStationRevision != "" {
		details["expected_station_revision"] = request.ExpectedStationRevision
	}
	if request.Name != "" {
		details["file"] = request.Name
	}
	if request.Content != nil {
		digest := sha256.Sum256([]byte(*request.Content))
		details["content_bytes"], details["content_sha256"] = len(*request.Content), hex.EncodeToString(digest[:])
	}
	return details
}

func unavailableLowerOperation() error {
	return &domain.OperationError{Code: "unavailable", Message: "下级当前未连接，不能执行运维操作"}
}

func normalizeLowerOperationError(ctx context.Context, err error, mutation bool) error {
	if err == nil {
		return nil
	}
	var operationErr *domain.OperationError
	if errors.As(err, &operationErr) {
		return err
	}
	message := "下级运维连接中断，请刷新后检查状态"
	if mutation {
		message += "；操作可能已生效，请勿直接重复提交"
	}
	code := "unavailable"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code, message = "timeout", "下级运维请求超时，请刷新状态核对结果"
		if mutation {
			message += "；操作可能已生效，请勿直接重复提交"
		}
	}
	return &domain.OperationError{Code: code, Message: message}
}

var _ api.LowerOperationsService = (*Runtime)(nil)
