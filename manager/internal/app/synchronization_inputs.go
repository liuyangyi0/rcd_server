package app

import (
	"context"
	"errors"
	"strings"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// Synchronization inputs are state refreshes, not hardware control commands.
// Their OPC success means only accepted by the lower's input cache. No ACK
// waiter, periodic replay, retry or command-audit row is created for each value.
func (r *Runtime) writeSynchronizationInput(ctx context.Context, sid string, key domain.NodeKey, value any) (domain.StatusCode, error) {
	parts := strings.SplitN(key.Identifier, ".", 4)
	if !opc.IsSynchronizationInput(key) {
		return domain.StatusBadConfigurationError, domain.ErrInvalid
	}
	port := ""
	if strings.HasPrefix(key.Identifier, "RcdSync.") {
		if len(parts) != 4 || parts[1] == "" || parts[2] != "TX" || parts[3] == "" {
			return domain.StatusBadConfigurationError, domain.ErrInvalid
		}
		port = parts[1]
	}
	r.configMu.Lock()
	reject := func(status domain.StatusCode, err error) (domain.StatusCode, error) {
		r.configMu.Unlock()
		return status, err
	}
	if r.closing.Load() {
		return reject(domain.StatusBadOutOfService, domain.ErrShuttingDown)
	}
	point, err := r.commandPointForSessionLocked(sid, key.Identifier)
	if err != nil {
		return reject(commandErrorStatus(err), err)
	}
	route, err := r.routes.Route(sid, point.GroupID)
	if err != nil {
		return reject(commandErrorStatus(err), err)
	}
	r.mu.RLock()
	node, exists := r.index.sourceNode(route.SourceID, key.Identifier)
	r.mu.RUnlock()
	if !exists || !node.Writable || node.ValueRank != -1 || node.Class != domain.NodeClassVariable || node.Key != key {
		return reject(domain.StatusBadConfigurationError, domain.ErrNodeNotConfigured)
	}
	valid := false
	switch node.DataType {
	case domain.DataTypeUInt32:
		_, valid = value.(uint32)
	case domain.DataTypeString:
		s, ok := value.(string)
		limit := 8192
		if key.Identifier == "TransmitSnapshot" || len(parts) == 4 && parts[0] == "RcdSync" && parts[3] == "Snapshot" {
			limit = 64 * 1024
		}
		valid = ok && len(s) <= limit
	}
	if !valid {
		return reject(domain.StatusBadConfigurationError, domain.ErrInvalid)
	}
	if err := r.checkProcessMaintenanceLocked(route.SourceID); err != nil {
		return reject(domain.StatusBadWaitingForResponse, err)
	}
	target, available := r.captureLowerCommandTarget(route.SourceID, port, false)
	if !available {
		return reject(domain.StatusBadNoCommunication, opc.ErrLowerUnavailable)
	}
	client, pinned := target.client.(opc.EpochPinnedLowerClient)
	if !pinned {
		return reject(domain.StatusBadConfigurationError, errors.New("synchronization writes require epoch-pinned lower client"))
	}
	// One reservation per input lets OPC's parallel multi-node Write update
	// different fields without falsely reporting a busy physical serial port.
	// Every in-flight input still fences group selection and maintenance.
	reservation, err := r.routes.BeginCommandWithDetails(sid, point.GroupID, "sync-input:"+key.Identifier, "SynchronizationInput", key.Identifier, "")
	if err != nil {
		return reject(commandErrorStatus(err), err)
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
	operation, cancel := context.WithTimeout(ctx, r.options.CommandTimeout)
	defer cancel()
	status, err := client.WriteAtEpoch(operation, key, value, target.localEpoch)
	if errors.Is(operation.Err(), context.DeadlineExceeded) {
		return domain.StatusBadTimeout, operation.Err()
	}
	if err != nil && status.IsGood() {
		status = domain.StatusBadNoCommunication
	}
	return status, err
}
