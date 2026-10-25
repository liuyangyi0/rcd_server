package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// OPCBackend keeps the two public protocol adapters decoupled: Runtime serves
// the HTTP API while this narrow wrapper implements opc.UpstreamBackend.
type OPCBackend struct{ Runtime *Runtime }

func (b OPCBackend) OpenSession(sessionID string) error { return b.Runtime.openSession(sessionID) }
func (b OPCBackend) CloseSession(sessionID string)      { b.Runtime.closeSession(sessionID) }
func (b OPCBackend) TouchSession(sessionID string) error {
	return b.Runtime.routes.TouchSession(sessionID)
}
func (b OPCBackend) ReadSample(sessionID string, key domain.NodeKey) domain.Sample {
	return b.Runtime.ReadSample(sessionID, key)
}
func (b OPCBackend) WriteCommand(ctx context.Context, sessionID string, key domain.NodeKey, value any) (domain.StatusCode, error) {
	return b.Runtime.WriteCommand(ctx, sessionID, key, value)
}
func (b OPCBackend) CallCommand(ctx context.Context, sessionID string, key domain.NodeKey, hexData string) (string, domain.StatusCode, error) {
	return b.Runtime.CallCommand(ctx, sessionID, key, hexData)
}
func (b OPCBackend) SelectSource(sessionID, groupID, sourceID string, revision uint64) (domain.GroupRoute, error) {
	return b.Runtime.selectSourceWithContext(context.Background(), sessionID, groupID, sourceID, revision, "opc")
}
func (b OPCBackend) SetDefaultSource(groupID, sourceID string, revision uint64) (domain.Group, error) {
	return b.Runtime.setDefault(context.Background(), groupID, sourceID, revision, "opc")
}
func (b OPCBackend) Session(sessionID string) (domain.Session, error) {
	return b.Runtime.routes.Session(sessionID)
}
func (b OPCBackend) Groups() []domain.Group   { return b.Runtime.routes.Groups() }
func (b OPCBackend) Sources() []domain.Source { return b.Runtime.routes.Sources() }
func (b OPCBackend) LowerStatus(sourceID string) (opc.LowerStatus, bool) {
	return b.Runtime.LowerStatus(sourceID)
}
func (b OPCBackend) SourcePointStatistics(sourceID string) opc.PointStatistics {
	return b.Runtime.SourcePointStatistics(sourceID)
}
func (b OPCBackend) GroupForNode(identifier string) (string, bool) {
	return b.Runtime.GroupForNode(identifier)
}
func (b OPCBackend) PendingCount(groupID string) int { return b.Runtime.routes.PendingCount(groupID) }
func (b OPCBackend) CommandQueueCounts(groupID string) (uint32, uint32) {
	return b.Runtime.routes.CommandQueueCounts(groupID)
}
func (b OPCBackend) GroupRedundancy(group string) (domain.RedundancyView, error) {
	return b.Runtime.redundancyDiagnostics(group)
}
func (b OPCBackend) SetSessionAutomatic(session, group string, automatic bool, revision uint64) (domain.GroupRoute, error) {
	return b.Runtime.SetSessionAutomatic(context.Background(), session, group, automatic, revision)
}
func (b OPCBackend) StationVersions(sourceID string) (domain.SourceStationVersions, bool) {
	return b.Runtime.StationVersions(sourceID)
}
func (b OPCBackend) StationVersionSupport(sourceID string) (bool, bool) {
	return b.Runtime.StationVersionSupport(sourceID)
}

func (b OPCBackend) StationCommunication(sourceID, portID, deviceID string) domain.StationCommunication {
	return b.Runtime.StationCommunication(sourceID, portID, deviceID)
}

func (b OPCBackend) CalculationMetadata() opc.CalculationMetadata {
	return b.Runtime.CalculationMetadata()
}

func (b OPCBackend) ReadGroupCommand(sessionID, groupID string) domain.Sample {
	return b.Runtime.ReadGroupCommand(sessionID, groupID)
}
func (b OPCBackend) ReadGroupCommandResult(sessionID, groupID string) domain.Sample {
	return b.Runtime.ReadGroupCommandResult(sessionID, groupID)
}
func (b OPCBackend) WriteGroupCommand(ctx context.Context, sessionID, groupID string, value any) (domain.StatusCode, error) {
	return b.Runtime.writeCommand(ctx, sessionID, domain.NodeKey{}, value, groupID)
}
func (b OPCBackend) CallGroupCommand(ctx context.Context, sessionID, groupID, hexData string) (string, domain.StatusCode, error) {
	return b.Runtime.callCommand(ctx, sessionID, domain.NodeKey{}, hexData, groupID)
}

var _ opc.UpstreamBackend = OPCBackend{}

func (r *Runtime) openSession(sessionID string) error {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return domain.ErrShuttingDown
	}
	if _, err := r.routes.Session(sessionID); err == nil {
		_, err = r.routes.OpenSession(sessionID)
		return err
	}
	if len(r.routes.Sessions()) >= r.options.MaxSessions {
		return fmt.Errorf("maximum %d upstream sessions reached", r.options.MaxSessions)
	}
	automatic := r.redundancy.automaticGroups()
	session, err := r.routes.OpenSessionWithAutomatic(sessionID, automatic)
	if err == nil {
		r.broker.Publish(api.Event{Type: "session.changed", Data: session})
	}
	return err
}

func (r *Runtime) closeSession(sessionID string) {
	r.routes.CloseSession(sessionID)
	r.mu.Lock()
	for key := range r.commandHistory {
		if strings.HasPrefix(key, sessionID+"\x00") {
			delete(r.commandHistory, key)
		}
	}
	r.mu.Unlock()
	r.broker.Publish(api.Event{Type: "session.changed", Data: map[string]string{"closed_id": sessionID}})
}

func (r *Runtime) selectSource(sessionID, groupID, sourceID string, revision uint64, actor string) (domain.GroupRoute, error) {
	return r.selectSourceWithContext(context.Background(), sessionID, groupID, sourceID, revision, actor)
}

func (r *Runtime) selectSourceWithContext(ctx context.Context, sessionID, groupID, sourceID string, revision uint64, actor string) (domain.GroupRoute, error) {
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return domain.GroupRoute{}, domain.ErrShuttingDown
	}
	route, err := r.routes.Select(sessionID, groupID, sourceID, revision)
	if err != nil {
		r.configMu.Unlock()
		return domain.GroupRoute{}, err
	}
	r.configMu.Unlock()
	r.queueSessionRefresh(sessionID, groupID)
	r.recordSelection(groupID)
	r.audit(ctx, "session.selection", sessionID+"/"+groupID, route.Revision,
		map[string]any{"source_id": sourceID, "actor": actor})
	r.broker.Publish(api.Event{Type: "selection.changed", Data: map[string]any{"session_id": sessionID, "route": route}})
	return route, nil
}

func (r *Runtime) ReadSample(sessionID string, key domain.NodeKey) domain.Sample {
	if sample, handled := r.calculationSample(key.Identifier); handled {
		return sample
	}
	if strings.HasSuffix(key.Identifier, "_Send") {
		if sample, handled := r.readLegacyCommand(sessionID, key.Identifier); handled {
			return sample
		}
	}
	point, ok := r.pointForIdentifier(key.Identifier)
	if !ok {
		return domain.Sample{StatusCode: domain.StatusBadConfigurationError}
	}
	route, err := r.routes.Route(sessionID, point.GroupID)
	if err != nil {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	if route.SourceID == "" {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	return r.sampleForSource(route.SourceID, point)
}

func (r *Runtime) sampleForSource(sourceID string, point indexedPoint) domain.Sample {
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	return r.sampleForSourceAtIndex(sourceID, point, index)
}

func (r *Runtime) sampleForSourceAtIndex(sourceID string, point indexedPoint, index *catalogIndex) domain.Sample {
	if point.Node.Kind == "manager_calculation" {
		if sample, handled := r.calculationSample(point.Node.Key.Identifier); handled {
			return sample
		}
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadWaitingInitialData}
	}
	if sourceID == "" {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	r.mu.RLock()
	status := r.statuses[sourceID]
	r.mu.RUnlock()
	if index == nil {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	// Catalog and node ownership come from one immutable publication. The
	// status view can briefly lag a hot configuration commit and is not the
	// authority for deciding whether that published catalog may be read.
	if index.union.SourceStates[sourceID] == domain.CatalogConflict {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	sourceNode, exists := index.sourceNode(sourceID, point.Node.Key.Identifier)
	if !exists {
		return domain.Sample{Type: point.Node.DataType, Value: zeroValue(point.Node.DataType, point.Node.ValueRank), StatusCode: domain.StatusBadConfigurationError}
	}
	if sourceNode.Retired || point.Node.Retired {
		return domain.Sample{Type: sourceNode.DataType, Value: zeroValue(sourceNode.DataType, sourceNode.ValueRank), StatusCode: domain.StatusBadOutOfService}
	}
	sample, hasSample := r.cache.Get(sourceID, sourceNode.Key)
	connected := status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive
	if !hasSample {
		code := domain.StatusBadWaitingInitialData
		if !connected {
			code = domain.StatusBadNoCommunication
		}
		return domain.Sample{Type: sourceNode.DataType, Value: zeroValue(sourceNode.DataType, sourceNode.ValueRank), StatusCode: code, Epoch: status.Epoch}
	}
	if connected && (status.Subscription != opc.SubscriptionActive || sample.Epoch != status.Epoch) {
		return domain.Sample{Type: sourceNode.DataType, Value: zeroValue(sourceNode.DataType, sourceNode.ValueRank),
			StatusCode: domain.StatusBadWaitingInitialData, Epoch: status.Epoch}
	}
	if sample.Type == "" {
		sample.Type = sourceNode.DataType
	}
	if !connected {
		sample.StatusCode = domain.StatusBadNoCommunication
	}
	// Never publish the cached process value as current data after its
	// communication path is lost. Keep its source timestamp for diagnostics,
	// but expose the declared type's zero value. Central calculations read this
	// same normalized sample and therefore continue evaluating with zero while
	// retaining BadNoCommunication quality.
	if sample.StatusCode.BaseCode() == domain.StatusBadNoCommunication && sourceNode.Kind != "calculation" {
		sample.Value = zeroValue(sourceNode.DataType, sourceNode.ValueRank)
	}
	return sample
}

func (r *Runtime) pointForIdentifier(identifier string) (indexedPoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index.pointForNode(identifier)
}

func (r *Runtime) WriteCommand(ctx context.Context, sessionID string, key domain.NodeKey, value any) (domain.StatusCode, error) {
	if opc.IsSynchronizationInput(key) {
		return r.writeSynchronizationInput(ctx, sessionID, key, value)
	}
	return r.writeCommand(ctx, sessionID, key, value, "")
}

func (r *Runtime) writeCommand(ctx context.Context, sessionID string, key domain.NodeKey, value any, groupHint string) (domain.StatusCode, error) {
	if groupHint == "" && (!strings.HasSuffix(key.Identifier, "_Send") || strings.HasSuffix(key.Identifier, "_SendCommand")) {
		return domain.StatusBadConfigurationError, domain.ErrInvalid
	}
	payload, ok := value.(string)
	if !ok {
		return domain.StatusBadConfigurationError, &domain.FieldError{Field: "value", Message: "must be a string"}
	}
	if passiveSendValue(payload) {
		return r.writePassiveSendValue(ctx, sessionID, key, payload, groupHint)
	}
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return domain.StatusBadOutOfService, domain.ErrShuttingDown
	}
	route, err := r.resolveCommandRouteLocked(sessionID, key.Identifier, payload, groupHint, "Write")
	if err != nil {
		r.configMu.Unlock()
		return commandErrorStatus(err), err
	}
	if err := r.checkProcessMaintenanceLocked(route.SourceID); err != nil {
		r.configMu.Unlock()
		return domain.StatusBadWaitingForResponse, err
	}
	port := route.Port
	// Freeze the topology mapping at acceptance; a later catalog must not
	// reinterpret the physical destination of an already queued payload.
	route.PhysicalPort = r.commandPhysicalPort(route)
	command, err := r.routes.EnqueueWrite(sessionID, route.GroupID, port, key.Identifier, payload)
	if err != nil {
		r.configMu.Unlock()
		return commandErrorStatus(err), err
	}
	target, targetOK := r.captureLowerCommandTarget(command.SourceID, port, true)
	r.rememberCommandRoute(sessionID, key.Identifier, route, command, target.globalEpoch)
	if targetOK {
		r.commands.register(command.ID, pendingCommandMeta{
			generation: target.generation, epoch: target.globalEpoch, baselineSequence: target.baselineSequence,
			queued: &queuedCommandDispatch{route: route, target: target},
		})
	}
	r.configMu.Unlock()
	// Publish Queued before dispatch. A fast lower source may publish its ACK
	// before Write returns, so publishing afterward could regress the audit feed.
	r.tracking.audit.record(command)
	r.broker.Publish(api.Event{Type: "command.changed", Data: command})
	if !targetOK {
		r.finishCommand(command.ID, domain.CommandFailed, domain.StatusBadNoCommunication, opc.ErrLowerUnavailable.Error())
		return domain.StatusBadNoCommunication, opc.ErrLowerUnavailable
	}
	r.commands.readyQueued(command.ID)
	// The first command preserves synchronous admission errors. Further writes
	// return Good once accepted, without waiting for the preceding hardware ACK.
	if active, dispatch, claimed := r.commands.claimQueued(command.SourceID, port, command.ID); claimed {
		return r.executeQueuedWrite(active, dispatch)
	}
	return domain.StatusGood, nil
}

func (r *Runtime) executeQueuedWrite(command domain.PendingCommand, dispatch queuedCommandDispatch) (domain.StatusCode, error) {
	r.releaseDispatches.Add(1)
	defer r.releaseDispatches.Add(-1)
	target, current := r.captureLowerCommandTarget(command.SourceID, command.Port, true)
	if r.closing.Load() || !current || target.globalEpoch != dispatch.target.globalEpoch || target.generation != dispatch.target.generation {
		r.finishCommand(command.ID, domain.CommandFailed, domain.StatusBadNoCommunication, "queued command target disconnected or changed; not sent")
		return domain.StatusBadNoCommunication, opc.ErrLowerUnavailable
	}
	remaining := time.Until(command.StartedAt.Add(r.options.CommandPendingTTL))
	if remaining <= 0 {
		r.finishCommand(command.ID, domain.CommandTimedOut, domain.StatusBadTimeout, "queued command expired before dispatch; not sent")
		return domain.StatusBadTimeout, context.DeadlineExceeded
	}
	if remaining > r.options.CommandTimeout {
		remaining = r.options.CommandTimeout
	}
	r.mu.RLock()
	parent := r.runContext
	r.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	operationContext, cancel := context.WithTimeout(parent, remaining)
	defer cancel()
	r.tracking.audit.record(command)
	r.broker.Publish(api.Event{Type: "command.changed", Data: command})
	status, err := r.dispatchCommand(operationContext, command, dispatch.route, dispatch.target, command.Request)
	if err != nil || !statusIsGood(status) {
		state := domain.CommandFailed
		if errors.Is(operationContext.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || status == domain.StatusBadTimeout {
			status = domain.StatusBadTimeout
			state = domain.CommandTimedOut
		}
		if statusIsGood(status) {
			status = domain.StatusBadNoCommunication
		}
		r.finishCommand(command.ID, state, status, commandDispatchError(status, err))
		return status, err
	}
	if terminal, _ := r.armCommand(command.ID); terminal != nil {
		r.finishCommand(command.ID, terminal.state, terminal.status, terminal.message)
	}
	return status, nil
}

func (r *Runtime) CallCommand(ctx context.Context, sessionID string, key domain.NodeKey, hexData string) (string, domain.StatusCode, error) {
	return r.callCommand(ctx, sessionID, key, hexData, "")
}

func (r *Runtime) callCommand(ctx context.Context, sessionID string, key domain.NodeKey, hexData, groupHint string) (string, domain.StatusCode, error) {
	if groupHint == "" && !strings.HasSuffix(key.Identifier, "_SendCommand") {
		return "", domain.StatusBadConfigurationError, domain.ErrInvalid
	}
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return "", domain.StatusBadOutOfService, domain.ErrShuttingDown
	}
	route, err := r.resolveCommandRouteLocked(sessionID, key.Identifier, hexData, groupHint, "Call")
	if err != nil {
		r.configMu.Unlock()
		return "", commandErrorStatus(err), err
	}
	if err := r.checkProcessMaintenanceLocked(route.SourceID); err != nil {
		r.configMu.Unlock()
		return "", domain.StatusBadWaitingForResponse, err
	}
	route.PhysicalPort = r.commandPhysicalPort(route)
	port := route.Port
	command, err := r.routes.BeginCommandWithDetails(sessionID, route.GroupID, port, "Call", key.Identifier, hexData)
	if err != nil {
		r.configMu.Unlock()
		return "", commandErrorStatus(err), err
	}
	r.releaseDispatches.Add(1)
	defer r.releaseDispatches.Add(-1)
	// A Method call must report the physical serial result, not merely that the
	// lower OPC server accepted a fire-and-forget RPC. Reuse the lower _Send
	// variable because it is the compatibility protocol that carries the
	// correlated 2 -> 1/3 device ACK state.
	target, targetOK := r.captureLowerCommandTarget(command.SourceID, port, true)
	r.rememberCommandRoute(sessionID, key.Identifier, route, command, target.globalEpoch)
	if targetOK {
		r.commands.register(command.ID, pendingCommandMeta{
			generation: target.generation, epoch: target.globalEpoch, baselineSequence: target.baselineSequence,
			done: make(chan pendingCommandTerminal, 1),
		})
	}
	r.configMu.Unlock()
	r.tracking.audit.record(command)
	r.broker.Publish(api.Event{Type: "command.changed", Data: command})
	if !targetOK {
		r.finishCommand(command.ID, domain.CommandFailed, domain.StatusBadNoCommunication, opc.ErrLowerUnavailable.Error())
		return "", domain.StatusBadNoCommunication, opc.ErrLowerUnavailable
	}
	operationContext, cancel := context.WithTimeout(ctx, r.options.CommandTimeout)
	defer cancel()
	status, err := r.dispatchCommand(operationContext, command, route, target, hexData)
	if err != nil || !statusIsGood(status) {
		state := domain.CommandFailed
		if errors.Is(operationContext.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || status == domain.StatusBadTimeout {
			state = domain.CommandTimedOut
			status = domain.StatusBadTimeout
		} else if statusIsGood(status) {
			status = domain.StatusBadNoCommunication
		}
		r.finishCommand(command.ID, state, status, commandDispatchError(status, err))
		return "", status, err
	}
	terminal, done := r.armCommand(command.ID)
	if terminal != nil {
		r.finishCommand(command.ID, terminal.state, terminal.status, terminal.message)
		return commandCallResult(*terminal)
	}
	select {
	case result := <-done:
		r.finishCommand(command.ID, result.state, result.status, result.message)
		return commandCallResult(result)
	case <-operationContext.Done():
		// Prefer an ACK that raced with the deadline over recording a false
		// timeout. The ACK observer signals before the caller persists the audit.
		select {
		case result := <-done:
			r.finishCommand(command.ID, result.state, result.status, result.message)
			return commandCallResult(result)
		default:
		}
		state := domain.CommandTimedOut
		status = domain.StatusBadTimeout
		message := "command timed out waiting for lower serial ACK"
		if !errors.Is(operationContext.Err(), context.DeadlineExceeded) {
			state = domain.CommandFailed
			status = domain.StatusBadUnexpectedError
			message = "command canceled while waiting for lower serial ACK"
		}
		r.finishCommand(command.ID, state, status, message)
		return "", status, operationContext.Err()
	}
}

func commandCallResult(terminal pendingCommandTerminal) (string, domain.StatusCode, error) {
	if terminal.state == domain.CommandReceived && terminal.status.IsGood() {
		return "1", terminal.status, nil
	}
	message := terminal.message
	if message == "" {
		message = "lower source rejected command"
	}
	return "", terminal.status, errors.New(message)
}

func (r *Runtime) completeCommandFromAck(sourceID, port string, sample domain.Sample, sequence uint64) {
	command, exists := r.routes.PendingCommand(sourceID, port)
	if !exists || (command.Kind != "Write" && command.Kind != "Call") || !sample.StatusCode.IsGood() {
		return
	}
	ack := strings.TrimSpace(fmt.Sprint(sample.Value))
	if ack != "1" && ack != "2" && ack != "3" {
		return
	}
	if ack == "2" {
		return
	}
	// The Rust lower can publish Waiting("2") and Received("1") inside one
	// 250 ms sampling window. QueueSize=1 then exposes only the terminal 1.
	// The lower adapter fences subscription delivery across its pre-write Read
	// and Write. Only timestamped transitions newer than that baseline enter this
	// sequence; observing 2 is useful but optional for a coalesced terminal ACK.
	// The ACK DataValue can carry lower-server notification information bits
	// (notably 0x80 when Waiting and Received share one publish interval).
	// Those bits describe the monitored-item queue, not the command outcome.
	// Persist a canonical Good status for a successful terminal ACK.
	terminal := pendingCommandTerminal{state: domain.CommandReceived, status: domain.StatusGood}
	if ack == "3" {
		terminal = pendingCommandTerminal{state: domain.CommandFailed, status: domain.StatusBadUnexpectedError, message: "lower source rejected command"}
	}
	if r.commands.observe(command.ID, terminal, false, sample.Epoch, sequence) {
		r.finishCommand(command.ID, terminal.state, terminal.status, terminal.message)
	}
}

func passiveSendValue(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "", "IDLE", "0", "1", "2", "3":
		return true
	default:
		return false
	}
}

func (r *Runtime) writePassiveSendValue(ctx context.Context, sessionID string, key domain.NodeKey, payload, groupHint string) (domain.StatusCode, error) {
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return domain.StatusBadOutOfService, domain.ErrShuttingDown
	}
	route, handled, err := r.resolveReadCommandRoute(sessionID, key.Identifier, groupHint)
	if !handled && err == nil {
		var point indexedPoint
		point, err = r.commandPointForSessionLocked(sessionID, key.Identifier)
		if err == nil {
			selected, routeErr := r.routes.Route(sessionID, point.GroupID)
			err = routeErr
			route = commandRoute{GroupID: point.GroupID, SourceID: selected.SourceID, Port: strings.TrimSuffix(key.Identifier, "_Send"), Key: key}
		}
	}
	if err != nil {
		r.configMu.Unlock()
		return commandErrorStatus(err), err
	}
	if err := r.checkProcessMaintenanceLocked(route.SourceID); err != nil {
		r.configMu.Unlock()
		return domain.StatusBadWaitingForResponse, err
	}
	if r.routes.HasPortCommands(route.SourceID, route.Port) {
		r.configMu.Unlock()
		return domain.StatusBadWaitingForResponse, domain.ErrCommandPending
	}
	target, targetOK := r.captureLowerCommandTarget(route.SourceID, route.Port, false)
	if !targetOK {
		r.configMu.Unlock()
		return domain.StatusBadNoCommunication, opc.ErrLowerUnavailable
	}
	// Reserve the same physical port and group as a hardware command before
	// releasing admission. A reset must not overtake a just-accepted command,
	// race its ACK variable, or disappear from a maintenance drain snapshot.
	// It has no hardware transaction or command audit and never enters the FIFO.
	reservation, err := r.routes.BeginCommandWithDetails(sessionID, route.GroupID, route.Port, domain.CommandKindPassiveReset, key.Identifier, "")
	if err != nil {
		r.configMu.Unlock()
		return commandErrorStatus(err), err
	}
	r.releaseDispatches.Add(1)
	r.configMu.Unlock()
	defer r.releaseDispatches.Add(-1)
	defer func() {
		changes, _ := r.routes.CommitCommandFinish(reservation.ID)
		for _, change := range changes {
			r.queueSessionRefresh(change.SessionID, change.Route.GroupID)
			r.broker.Publish(api.Event{Type: "selection.changed", Data: change})
		}
	}()
	operationContext, cancel := context.WithTimeout(ctx, r.options.CommandTimeout)
	defer cancel()
	status, err := writePinned(operationContext, target, route.Key, payload)
	if errors.Is(operationContext.Err(), context.DeadlineExceeded) {
		return domain.StatusBadTimeout, operationContext.Err()
	}
	if err != nil && status.IsGood() {
		status = domain.StatusBadNoCommunication
	}
	if err == nil && status.IsGood() {
		r.rememberPassiveCommand(sessionID, key.Identifier, groupHint, route, payload)
	}
	return status, err
}

func (r *Runtime) armCommand(commandID string) (*pendingCommandTerminal, <-chan pendingCommandTerminal) {
	return r.commands.arm(commandID)
}

func (r *Runtime) finishCommand(id string, state domain.CommandState, status domain.StatusCode, message string) {
	command, routeChanges, cancelled, err := r.commands.finish(id, state, status, message, r.finishCommandHistory)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return
		}
		slog.Error("release command", "command_id", command.ID, "error", err)
		return
	}
	r.tracking.audit.record(command)
	for _, queued := range cancelled {
		r.finishCommandHistory(queued)
		r.tracking.audit.record(queued)
		r.broker.Publish(api.Event{Type: "command.changed", Data: queued})
	}
	for _, change := range routeChanges {
		r.queueSessionRefresh(change.SessionID, change.Route.GroupID)
		r.broker.Publish(api.Event{Type: "selection.changed", Data: change})
	}
	r.broker.Publish(api.Event{Type: "command.changed", Data: command})
	// An unconfirmed timeout/write failure must not release a burst of old
	// commands into a port whose previous transaction may still be running.
	r.startQueuedWrite(command.SourceID, command.Port)
}

func (r *Runtime) retryDirtyCommands() {
	r.tracking.audit.flush()
}

type lowerCommandTarget struct {
	client           opc.LowerClient
	generation       uint64
	globalEpoch      uint64
	localEpoch       uint64
	baselineSequence uint64
}

func (r *Runtime) captureLowerCommandTarget(sourceID, port string, requireSubscription bool) (lowerCommandTarget, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handle, ok := r.connections.handle(sourceID)
	if !ok {
		return lowerCommandTarget{}, false
	}
	status := r.statuses[sourceID]
	if status.Connection != opc.ConnectionConnected || status.Session != opc.SessionActive ||
		(requireSubscription && status.Subscription != opc.SubscriptionActive) {
		return lowerCommandTarget{}, false
	}
	ackKey := sourceID + "\x00" + port
	ackEpoch, ackObserved := r.ackEpochs[ackKey]
	// A newly created subscription publishes the retained _Send value
	// asynchronously.  Do not dispatch a write until that value has passed
	// through OnSample for this exact connection epoch; otherwise a delayed
	// retained "3" could be mistaken for the new command's immediate failure.
	if requireSubscription && (!ackObserved || ackEpoch != status.Epoch) {
		return lowerCommandTarget{}, false
	}
	localEpoch := status.Epoch
	if status.Epoch>>32 == handle.generation {
		localEpoch = status.Epoch & 0xffffffff
	}
	return lowerCommandTarget{
		client: handle.client, generation: handle.generation, globalEpoch: status.Epoch, localEpoch: localEpoch,
		baselineSequence: r.ackSequences[ackKey],
	}, true
}

func writePinned(ctx context.Context, target lowerCommandTarget, key domain.NodeKey, value any) (domain.StatusCode, error) {
	if client, ok := target.client.(opc.EpochPinnedLowerClient); ok {
		return client.WriteAtEpoch(ctx, key, value, target.localEpoch)
	}
	return target.client.Write(ctx, key, value)
}

func callPinned(ctx context.Context, target lowerCommandTarget, key domain.NodeKey, value string) (string, domain.StatusCode, error) {
	if client, ok := target.client.(opc.EpochPinnedLowerClient); ok {
		return client.CallAtEpoch(ctx, key, value, target.localEpoch)
	}
	return target.client.Call(ctx, key, value)
}

func (r *Runtime) commandSweeper() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	interval := r.options.CommandPendingTTL / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	eventFlushTicker := time.NewTicker(100 * time.Millisecond)
	defer eventFlushTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-eventFlushTicker.C:
			r.flushPendingSampleEvent(now)
		case <-r.statusPersistWake:
			r.retryDirtyStatuses(ctx)
		case now := <-ticker.C:
			r.retryDirtyCommands()
			r.sweepExpiredCommands(now)
			r.retryDirtyCatalogs(ctx)
			r.retryDirtyAudits(ctx)
			r.retryDirtyStatuses(ctx)
			r.retryDirtyUpstream()
			r.retryMissingLowers()
		}
	}
}

func (r *Runtime) sweepExpiredCommands(now time.Time) {
	r.cancelDisconnectedQueued()
	for _, command := range r.routes.ExpiredPending(now, r.options.CommandPendingTTL) {
		r.finishCommand(command.ID, domain.CommandTimedOut, domain.StatusBadTimeout, "command timed out")
	}
}

func (r *Runtime) retryMissingLowers() {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() || !r.running.Load() {
		return
	}
	for _, source := range r.routes.Sources() {
		if !source.Enabled {
			continue
		}
		r.mu.RLock()
		_, exists := r.connections.handle(source.ID)
		r.mu.RUnlock()
		if exists {
			continue
		}
		if err := r.startLower(source); err != nil {
			slogSourceError("retry lower source start", source.ID, err)
			r.recordLowerStartFailure(source, err)
		}
	}
}

func (r *Runtime) retryDirtyUpstream() {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return
	}
	dirty, _ := r.publication.status()
	if !dirty {
		return
	}
	if err := r.reconcileUpstream(); err != nil {
		slog.Error("retry upstream catalog reconciliation", "error", err)
	}
}

func (r *Runtime) retryDirtyAudits(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if r.managementAudit.retry(ctx, r.repo.AppendAudit) {
		r.broker.Publish(api.Event{Type: "management.audit"})
	}
}

func (r *Runtime) drainManagementAudits(ctx context.Context) {
	if r.managementAudit.drain(ctx, r.repo.AppendAudit) {
		r.broker.Publish(api.Event{Type: "management.audit"})
	}
}

func (r *Runtime) retryDirtyCatalogs(ctx context.Context) {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return
	}
	r.persistDirtyCatalogs(ctx)
}

func (r *Runtime) persistDirtyCatalogs(ctx context.Context) {
	r.catalogPersistence.flush(ctx)
}

func (r *Runtime) lowerClient(sourceID string) (opc.LowerClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handle, ok := r.connections.handle(sourceID)
	if !ok {
		return nil, false
	}
	status := r.statuses[sourceID]
	if status.Connection != opc.ConnectionConnected {
		return nil, false
	}
	return handle.client, true
}

func (r *Runtime) LowerStatus(sourceID string) (opc.LowerStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status, ok := r.statuses[sourceID]
	return status, ok
}

func (r *Runtime) SourcePointStatistics(sourceID string) opc.PointStatistics {
	r.mu.RLock()
	index := r.index
	catalog := r.catalogs[sourceID]
	nodes := make([]domain.CatalogNode, 0)
	if index != nil {
		if indexedNodes, exists := index.sourceNodes[sourceID]; exists {
			nodes = make([]domain.CatalogNode, 0, len(indexedNodes))
			for _, node := range indexedNodes {
				nodes = append(nodes, node)
			}
		}
	}
	if len(nodes) == 0 {
		nodes = append(nodes, catalog.Nodes...)
	}
	r.mu.RUnlock()

	stats := opc.PointStatistics{}
	for _, node := range nodes {
		if node.Class != domain.NodeClassVariable {
			continue
		}
		stats.Total++
		if index == nil {
			stats.Bad++
			continue
		}
		point, exists := index.pointByNode[node.Key.Identifier]
		if !exists || !containsString(point.SourceIDs, sourceID) {
			stats.Bad++
			continue
		}
		if statusIsGood(r.sampleForSourceAtIndex(sourceID, point, index).StatusCode) {
			stats.Good++
		} else {
			stats.Bad++
		}
	}
	return stats
}

func (r *Runtime) GroupForNode(identifier string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index.groupForNode(identifier)
}

func commandErrorStatus(err error) domain.StatusCode {
	switch {
	case errors.Is(err, domain.ErrCommandPending), errors.Is(err, domain.ErrGroupPending):
		return domain.StatusBadWaitingForResponse
	case errors.Is(err, domain.ErrSessionClosed), errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrSourceNotSelectable), errors.Is(err, domain.ErrNodeNotConfigured):
		return domain.StatusBadConfigurationError
	case errors.Is(err, domain.ErrNodeRetired):
		return domain.StatusBadOutOfService
	case errors.Is(err, domain.ErrShuttingDown):
		return domain.StatusBadOutOfService
	default:
		return domain.StatusBadConfigurationError
	}
}

// commandPointForSessionLocked must be called while configMu is held. It closes
// the catalog-to-dispatch TOCTOU window and enforces the same union-missing and
// hot-retired rules used by ReadSample before a pending command is created.
func (r *Runtime) commandPointForSessionLocked(sessionID, identifier string) (indexedPoint, error) {
	point, ok := r.pointForIdentifier(identifier)
	if !ok {
		return indexedPoint{}, domain.ErrNotFound
	}
	if point.Node.Retired {
		return indexedPoint{}, domain.ErrNodeRetired
	}
	session, err := r.routes.Session(sessionID)
	if err != nil {
		return indexedPoint{}, err
	}
	route, ok := session.Routes[point.GroupID]
	if !ok || route.SourceID == "" {
		return indexedPoint{}, domain.ErrNodeNotConfigured
	}
	r.mu.RLock()
	sourceNode, exists := r.index.sourceNode(route.SourceID, identifier)
	r.mu.RUnlock()
	if !exists {
		return indexedPoint{}, domain.ErrNodeNotConfigured
	}
	if sourceNode.Retired {
		return indexedPoint{}, domain.ErrNodeRetired
	}
	return point, nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func commandDispatchError(status domain.StatusCode, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("lower source rejected command with status 0x%08X", uint32(status))
}
