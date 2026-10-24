package app

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

const unverifiedStationTopology = "unverified"

func (r *Runtime) cacheStationVersionResult(request domain.OperationRequest, result json.RawMessage, source domain.Source, connectionEpoch uint64, fingerprint string) error {
	defer r.refreshCalculationStateInputs(source.ID)
	r.stationSync.writerMu.Lock()
	defer r.stationSync.writerMu.Unlock()
	verified := fingerprint != "" && fingerprint != unverifiedStationTopology
	if fingerprint == "" {
		fingerprint = unverifiedStationTopology
	}
	snapshot, present, err := opc.DecodeStationVersionStatus(request, result, source.ID, fingerprint, time.Now().UTC())
	if err != nil || !present {
		return err
	}
	snapshot.Cached = false
	r.mu.Lock()
	currentFingerprint := domain.SourceTopologyFingerprint(r.catalogs[source.ID].Topology)
	if currentFingerprint == "" {
		currentFingerprint = unverifiedStationTopology
	}
	if current, ok := r.statuses[source.ID]; !ok || current.Epoch != connectionEpoch || currentFingerprint != fingerprint {
		// A Call issued on an old downstream Session may complete after the
		// supervisor has attached a new one. Never let that late response update
		// the live cache or SQLite state for the new connection epoch.
		r.mu.Unlock()
		return nil
	}
	previous, previousEpoch, previousLive, existed := r.stations.read(source.ID)
	if existed && previousLive && previous.TopologyFingerprint == snapshot.TopologyFingerprint && previousEpoch == connectionEpoch {
		previousRevision, previousOK := domain.ParseStationRevision(previous.StationRevision)
		incomingRevision, incomingOK := domain.ParseStationRevision(snapshot.StationRevision)
		if previousOK && incomingOK && incomingRevision < previousRevision {
			r.mu.Unlock()
			return nil
		}
	}
	if existed && sameStationVersionSnapshot(previous, snapshot) && !r.stationSync.needsPersistence(source.ID) {
		r.stations.touch(source.ID, snapshot.ObservedAt, connectionEpoch)
		r.stationSync.setSupport(source.ID, connectionEpoch, true)
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	persistedSnapshot := false
	// Acquisition flags change independently of station/version revisions.
	// Keep their updates in memory; a health refresh is not a SQLite data feed.
	if verified && (r.stationSync.needsPersistence(source.ID) || !existed || !samePersistedStationVersionSnapshot(previous, snapshot)) {
		persistContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		persistErr := r.repo.SaveSourceStationVersions(persistContext, snapshot)
		cancel()
		r.stationSync.persistenceResult(source.ID, persistErr)
		if persistErr != nil {
			// The live immutable snapshot remains useful, but health reports that
			// it cannot yet survive a manager restart.
			err = persistErr
		} else {
			persistedSnapshot = true
		}
	}
	r.mu.Lock()
	currentFingerprint = domain.SourceTopologyFingerprint(r.catalogs[source.ID].Topology)
	if currentFingerprint == "" {
		currentFingerprint = unverifiedStationTopology
	}
	if current, ok := r.statuses[source.ID]; !ok || current.Epoch != connectionEpoch || currentFingerprint != fingerprint {
		r.mu.Unlock()
		// Persistence may have overlapped a reconnect. Roll back the just-written
		// row as well as suppressing memory/SSE publication; a newly attached old
		// lower may not support station_versions and therefore never overwrite it.
		if persistedSnapshot {
			if rollbackErr := r.restoreStationVersionPersistence(source.ID, previous, existed); rollbackErr != nil {
				err = rollbackErr
			}
		}
		r.queueStationVersionRefresh(source.ID)
		return err
	}
	r.stations.replace(source.ID, snapshot, connectionEpoch, true)
	r.stationSync.setSupport(source.ID, connectionEpoch, true)
	r.mu.Unlock()
	r.broker.Publish(api.Event{Type: "station_versions_changed", SourceID: source.ID,
		Data: map[string]any{"source_id": source.ID, "station_revision": snapshot.StationRevision}})
	return err
}

func (r *Runtime) restoreStationVersionPersistence(sourceID string, previous domain.SourceStationVersions, existed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if existed && previous.TopologyFingerprint != "" && previous.TopologyFingerprint != unverifiedStationTopology &&
		domain.ValidOperationRevision(previous.Revision) && domain.ValidStationRevision(previous.StationRevision) {
		err = r.repo.SaveSourceStationVersions(ctx, previous)
	} else {
		err = r.repo.ClearSourceStationVersions(ctx, sourceID)
	}
	r.stationSync.persistenceResult(sourceID, err)
	return err
}

func sameStationVersionSnapshot(left, right domain.SourceStationVersions) bool {
	left.ObservedAt, right.ObservedAt = time.Time{}, time.Time{}
	left.Cached, right.Cached = false, false
	return reflect.DeepEqual(left, right)
}

func samePersistedStationVersionSnapshot(left, right domain.SourceStationVersions) bool {
	stripRuntime := func(snapshot domain.SourceStationVersions) domain.SourceStationVersions {
		snapshot = snapshot.Clone()
		for i := range snapshot.Ports {
			port := &snapshot.Ports[i]
			port.AppliedEnabled, port.Open, port.ControlPending, port.PendingCommands = false, false, false, 0
			for j := range port.Devices {
				device := &port.Devices[j]
				device.AppliedReadEnabled, device.CommunicationOK, device.Stale = false, false, false
			}
		}
		return snapshot
	}
	return sameStationVersionSnapshot(stripRuntime(left), stripRuntime(right))
}

func (r *Runtime) StationVersions(sourceID string) (domain.SourceStationVersions, bool) {
	return r.stations.snapshot(sourceID)
}

func (r *Runtime) StationVersionSupport(sourceID string) (known, supported bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status, hasStatus := r.statuses[sourceID]
	if !hasStatus {
		return false, false
	}
	return r.stationSync.support(sourceID, status.Epoch)
}

func (r *Runtime) queueStationVersionRefresh(sourceID string) {
	r.mu.RLock()
	ctx := r.runContext
	_, exists := r.statuses[sourceID]
	if ctx == nil || !r.running.Load() || !exists {
		r.mu.RUnlock()
		return
	}
	ticket, enqueue := r.stationSync.request(sourceID)
	r.mu.RUnlock()
	if !enqueue {
		return
	}
	select {
	case r.stationSync.queue() <- ticket:
	case <-ctx.Done():
		r.stationSync.finish(ticket)
	default:
		// The periodic sweep will retry; release only this exact reservation.
		r.stationSync.finish(ticket)
	}
}

func (r *Runtime) stationVersionRefreshWorker() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	// Runtime acquisition health does not increment StationVersionRevision.
	// Poll status separately from the hot point path. A bounded pool prevents
	// one unresponsive lower from blocking every other source's health.
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ticket := <-r.stationSync.queue():
					if !r.stationSync.current(ticket) {
						continue
					}
					refreshContext, cancel := context.WithTimeout(ctx, 3*time.Second)
					_ = r.refreshSourceStationVersions(refreshContext, ticket.sourceID)
					cancel()
					// Yield the worker after every attempt, including a dirty
					// refresh, so other sources cannot be starved indefinitely.
					if r.stationSync.finish(ticket) {
						r.queueStationVersionRefresh(ticket.sourceID)
					}
				}
			}
		}()
	}
	defer workers.Wait()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.RLock()
			sources := make([]string, 0, len(r.statuses))
			for sourceID, status := range r.statuses {
				if status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive && status.Subscription == opc.SubscriptionActive {
					sources = append(sources, sourceID)
				}
			}
			r.mu.RUnlock()
			for _, sourceID := range sources {
				r.queueStationVersionRefresh(sourceID)
			}
		}
	}
}

func (r *Runtime) refreshSourceStationVersions(ctx context.Context, sourceID string) error {
	r.mu.RLock()
	status, exists := r.statuses[sourceID]
	handle, attached := r.connections.handle(sourceID)
	fingerprint := domain.SourceTopologyFingerprint(r.catalogs[sourceID].Topology)
	known, supported := r.stationSync.support(sourceID, status.Epoch)
	r.mu.RUnlock()
	if !exists || !attached || status.Connection != opc.ConnectionConnected || status.Session != opc.SessionActive {
		return nil
	}
	if !known {
		capabilities, err := r.SourceOperationsCapabilities(ctx, sourceID)
		if err != nil {
			return err
		}
		supported = capabilities.Supported && capabilities.HasFeature("station_versions")
		r.mu.Lock()
		current, exists := r.statuses[sourceID]
		if !exists || current.Epoch != status.Epoch ||
			current.Connection != opc.ConnectionConnected || current.Session != opc.SessionActive ||
			!r.connections.current(sourceID, handle.generation) ||
			domain.SourceTopologyFingerprint(r.catalogs[sourceID].Topology) != fingerprint {
			r.mu.Unlock()
			return nil
		}
		r.stationSync.setSupport(sourceID, status.Epoch, supported)
		r.mu.Unlock()
		if !supported {
			r.broker.Publish(api.Event{Type: "station_versions_changed", SourceID: sourceID,
				Data: map[string]any{"source_id": sourceID, "supported": false}})
		}
	}
	if !supported {
		return nil
	}
	_, err := r.ExecuteSourceOperation(ctx, sourceID, 0, domain.OperationRequest{Action: "status"})
	return err
}

func (r *Runtime) currentStationStatus(ctx context.Context, sourceID string) (domain.SourceStationVersions, error) {
	result, err := r.ExecuteSourceOperation(ctx, sourceID, 0, domain.OperationRequest{Action: "status"})
	if err != nil {
		return domain.SourceStationVersions{}, err
	}
	r.mu.RLock()
	topology := r.catalogs[sourceID].Topology.Clone()
	r.mu.RUnlock()
	fingerprint := domain.SourceTopologyFingerprint(topology)
	if fingerprint == "" {
		fingerprint = unverifiedStationTopology
	}
	snapshot, present, err := opc.DecodeStationVersionStatus(domain.OperationRequest{Action: "status"}, result, sourceID, fingerprint, time.Now().UTC())
	if err != nil {
		return domain.SourceStationVersions{}, err
	}
	if !present {
		return domain.SourceStationVersions{}, &domain.OperationError{Code: "unsupported", Message: "该下级未提供设备版本管理能力"}
	}
	return snapshot, nil
}

func (r *Runtime) SetDeviceModuleType(ctx context.Context, sourceID string, mutation api.DeviceVersionMutation) (json.RawMessage, error) {
	return r.executeDeviceVersionMutation(ctx, sourceID, mutation, "set_device_module_type")
}

func (r *Runtime) QueryDeviceVersion(ctx context.Context, sourceID string, mutation api.DeviceVersionMutation) (json.RawMessage, error) {
	return r.executeDeviceVersionMutation(ctx, sourceID, mutation, "query_device_version")
}

func (r *Runtime) executeDeviceVersionMutation(ctx context.Context, sourceID string, mutation api.DeviceVersionMutation, action string) (json.RawMessage, error) {
	if mutation.SourceRevision == 0 || !domain.ValidStationRevision(mutation.StationRevision) {
		return nil, &domain.FieldError{Field: "revision", Message: "source_revision and station_revision are required"}
	}
	r.configMu.RLock()
	source, sourceErr := r.source(sourceID)
	r.configMu.RUnlock()
	if sourceErr != nil {
		return nil, sourceErr
	}
	if source.Revision != mutation.SourceRevision {
		return nil, &domain.RevisionConflictError{Expected: mutation.SourceRevision, Actual: source.Revision}
	}
	status, err := r.currentStationStatus(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if status.StationRevision != mutation.StationRevision {
		return nil, &domain.OperationError{Code: "conflict", Message: "设备版本状态已变化，请刷新后重试", CurrentRevision: status.StationRevision}
	}
	port, device, found := status.Device(mutation.PortID, mutation.DeviceID)
	if !found {
		return nil, &domain.OperationError{Code: "not_found", Message: "未找到指定串口站点"}
	}
	if action == "query_device_version" && device.ConfiguredModuleType != domain.ModuleTypeDomestic {
		return nil, &domain.OperationError{Code: "conflict", Message: "只有配置为国产模块的站点才能查询版本"}
	}
	if action == "query_device_version" {
		if port.ControlPending || port.PendingCommands > 0 {
			return nil, &domain.OperationError{Code: "busy", Message: "该串口有控制或命令正在执行"}
		}
		if !port.Enabled || !port.AppliedEnabled || !port.Open || !device.ReadEnabled || !device.AppliedReadEnabled {
			return nil, &domain.OperationError{Code: "conflict", Message: "串口和站点读取必须已启用且已应用后才能查询版本"}
		}
	}
	request := domain.OperationRequest{
		Action: action, PortID: mutation.PortID, DeviceID: mutation.DeviceID,
		ExpectedRevision: status.Revision, ExpectedStationRevision: mutation.StationRevision,
	}
	if action == "set_device_module_type" {
		request.ModuleType = mutation.ModuleType
	}
	return r.ExecuteSourceOperation(ctx, sourceID, mutation.SourceRevision, request)
}

func (r *Runtime) ListDeviceVersions(_ context.Context, request api.DeviceVersionRequest) (api.DeviceVersionPage, error) {
	if request.Page < 1 {
		request.Page = 1
	}
	if request.PageSize < 1 || request.PageSize > 1000 {
		request.PageSize = 100
	}
	sources := r.routes.Sources()
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	r.mu.RLock()
	stationView := r.stations.view()
	statuses := make(map[string]opc.LowerStatus, len(r.statuses))
	featureSupport := make(map[string]bool, len(r.statuses))
	featureKnown := make(map[string]bool, len(r.statuses))
	topologies := make(map[string]*domain.SourceTopology, len(r.catalogs))
	for id, status := range r.statuses {
		statuses[id] = status
	}
	for id, status := range statuses {
		featureKnown[id], featureSupport[id] = r.stationSync.support(id, status.Epoch)
	}
	for id, catalog := range r.catalogs {
		topologies[id] = catalog.Topology.Clone()
	}
	r.mu.RUnlock()
	snapshots := stationView.cloneAll()
	items := make([]api.DeviceVersionItem, 0)
	for _, source := range sources {
		if request.SourceID != "" && source.ID != request.SourceID {
			continue
		}
		status := statuses[source.ID]
		online := status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive
		snapshot, hasSnapshot := snapshots[source.ID]
		portSnapshots := make(map[string]domain.StationVersionPort)
		deviceSnapshots := make(map[string]domain.StationVersion)
		for _, port := range snapshot.Ports {
			portSnapshots[port.PortID] = port
			for _, device := range port.Devices {
				deviceSnapshots[port.PortID+"\x00"+device.DeviceID] = device
			}
		}
		topology := topologies[source.ID]
		if topology != nil {
			for _, port := range topology.Ports {
				portSnapshot, ok := portSnapshots[port.PortID]
				if !ok {
					portSnapshot = domain.StationVersionPort{PortID: port.PortID, PortName: port.PortName}
					portSnapshots[port.PortID] = portSnapshot
				}
				for _, device := range port.Devices {
					key := port.PortID + "\x00" + device.DeviceID
					if _, ok := deviceSnapshots[key]; !ok {
						deviceSnapshots[key] = domain.StationVersion{DeviceID: device.DeviceID, DeviceName: device.Name,
							ConfiguredModuleType: domain.ModuleTypeUnknown, VersionState: "waiting"}
					}
				}
			}
		}
		keys := make([]string, 0, len(deviceSnapshots))
		for key := range deviceSnapshots {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left, right := strings.SplitN(keys[i], "\x00", 2), strings.SplitN(keys[j], "\x00", 2)
			if left[0] != right[0] {
				return left[0] < right[0]
			}
			li, le := strconv.Atoi(left[1])
			ri, re := strconv.Atoi(right[1])
			if le == nil && re == nil {
				return li < ri
			}
			return left[1] < right[1]
		})
		for _, key := range keys {
			parts := strings.SplitN(key, "\x00", 2)
			port, device := portSnapshots[parts[0]], deviceSnapshots[key]
			versionSupported := domain.ValidStationRevision(snapshot.StationRevision)
			if featureKnown[source.ID] && !featureSupport[source.ID] {
				versionSupported = false
			}
			item := api.DeviceVersionItem{
				SourceID: source.ID, SourceName: source.Name, SourceRevision: source.Revision,
				RCDSoftwareVersion: snapshot.SoftwareVersion, RCDBuildCommit: snapshot.BuildCommit,
				PortID: port.PortID, PortName: port.PortName, DeviceID: device.DeviceID, DeviceName: device.DeviceName,
				ConfiguredModuleType: device.ConfiguredModuleType, DetectedModuleType: device.DetectedModuleType,
				TypeMismatch: device.TypeMismatch, CommunicationModuleVersion: device.CommunicationModuleVersion,
				CommunicationModuleVersionRaw: device.CommunicationModuleVersionRaw,
				PLCVersion:                    device.PLCVersion, PLCVersionRaw: device.PLCVersionRaw, VersionPayload: device.VersionPayload,
				VersionState: device.VersionState, LastVersionAttemptAt: device.LastVersionAttemptAt,
				LastVersionSuccessAt: device.LastVersionSuccessAt, LastVersionError: device.LastVersionError,
				ConsecutiveFailures: device.ConsecutiveFailures, StationRevision: snapshot.StationRevision,
				VersionSupported: versionSupported,
				SourceOnline:     online, Cached: hasSnapshot && (snapshot.Cached || !online), Stale: device.Stale || (hasSnapshot && !online),
				PortEnabled: port.Enabled, PortAppliedEnabled: port.AppliedEnabled, PortOpen: port.Open,
				DeviceReadEnabled: device.ReadEnabled, AppliedDeviceReadEnabled: device.AppliedReadEnabled,
				CommunicationOK: device.CommunicationOK, ControlPending: port.ControlPending,
				PendingCommands: port.PendingCommands, ObservedAt: snapshot.ObservedAt,
			}
			if !hasSnapshot && !online {
				item.VersionState = "offline"
			} else if !hasSnapshot && featureKnown[source.ID] && !featureSupport[source.ID] {
				item.VersionState = "unsupported"
			}
			if deviceVersionMatches(item, request) {
				items = append(items, item)
			}
		}
	}
	summary := summarizeDeviceVersions(items)
	total := len(items)
	start := (request.Page - 1) * request.PageSize
	if start > total {
		start = total
	}
	end := start + request.PageSize
	if end > total {
		end = total
	}
	return api.DeviceVersionPage{Items: items[start:end], Page: request.Page, PageSize: request.PageSize, Total: total, Summary: summary}, nil
}

func deviceVersionMatches(item api.DeviceVersionItem, request api.DeviceVersionRequest) bool {
	if request.PortID != "" && item.PortID != request.PortID {
		return false
	}
	if request.ModuleType != "" && item.ConfiguredModuleType != request.ModuleType {
		return false
	}
	if request.Status != "" {
		status := strings.ToLower(request.Status)
		switch status {
		case "mismatch":
			if !item.TypeMismatch {
				return false
			}
		case "offline_cached":
			if item.SourceOnline || !item.Cached {
				return false
			}
		case "stale":
			if !item.Stale {
				return false
			}
		default:
			if strings.ToLower(item.VersionState) != status {
				return false
			}
		}
	}
	query := strings.ToLower(strings.TrimSpace(request.Query))
	if query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{item.SourceID, item.SourceName, item.PortID, item.PortName,
		item.DeviceID, item.DeviceName, item.ConfiguredModuleType, item.DetectedModuleType,
		item.CommunicationModuleVersion, item.PLCVersion, item.VersionState, item.LastVersionError}, "\x00"))
	return strings.Contains(haystack, query)
}

func summarizeDeviceVersions(items []api.DeviceVersionItem) api.DeviceVersionSummary {
	summary := api.DeviceVersionSummary{Total: len(items), LowerSoftwareVersions: []api.LowerSoftwareVersionSummary{}}
	type buildIdentity struct{ version, commit string }
	builds := make(map[buildIdentity]map[string]struct{})
	for _, item := range items {
		switch item.ConfiguredModuleType {
		case domain.ModuleTypeDomestic:
			summary.Domestic++
		case domain.ModuleTypeLegacyRussian:
			summary.LegacyRussian++
		default:
			summary.Unknown++
		}
		if item.TypeMismatch {
			summary.Mismatch++
		}
		state := strings.ToLower(item.VersionState)
		if state == "failed" || state == "error" || state == "timeout" {
			summary.Failed++
		}
		if !item.SourceOnline && item.Cached {
			summary.OfflineCached++
		}
		if item.RCDSoftwareVersion != "" || item.RCDBuildCommit != "" {
			identity := buildIdentity{version: item.RCDSoftwareVersion, commit: item.RCDBuildCommit}
			if builds[identity] == nil {
				builds[identity] = make(map[string]struct{})
			}
			builds[identity][item.SourceID] = struct{}{}
		}
	}
	for identity, sources := range builds {
		summary.LowerSoftwareVersions = append(summary.LowerSoftwareVersions, api.LowerSoftwareVersionSummary{
			SoftwareVersion: identity.version, BuildCommit: identity.commit, SourceCount: len(sources),
		})
	}
	sort.Slice(summary.LowerSoftwareVersions, func(i, j int) bool {
		left, right := summary.LowerSoftwareVersions[i], summary.LowerSoftwareVersions[j]
		if left.SoftwareVersion != right.SoftwareVersion {
			return left.SoftwareVersion < right.SoftwareVersion
		}
		return left.BuildCommit < right.BuildCommit
	})
	return summary
}

func stationOperationAuditResult(result json.RawMessage, request domain.OperationRequest) map[string]any {
	var snapshot struct {
		StationRevision string `json:"station_revision"`
		Ports           []struct {
			PortID  string `json:"port_id"`
			Devices []struct {
				DeviceID                   string `json:"device_id"`
				ConfiguredModuleType       string `json:"configured_module_type"`
				DetectedModuleType         string `json:"detected_module_type"`
				TypeMismatch               bool   `json:"type_mismatch"`
				CommunicationModuleVersion string `json:"communication_module_version"`
				PLCVersion                 string `json:"plc_version"`
				VersionState               string `json:"version_state"`
				LastVersionError           string `json:"last_version_error"`
			} `json:"devices"`
		} `json:"ports"`
	}
	if json.Unmarshal(result, &snapshot) != nil {
		return nil
	}
	details := map[string]any{"station_revision": snapshot.StationRevision}
	for _, port := range snapshot.Ports {
		if port.PortID != request.PortID {
			continue
		}
		for _, device := range port.Devices {
			if device.DeviceID != request.DeviceID {
				continue
			}
			details["configured_module_type"] = device.ConfiguredModuleType
			details["detected_module_type"] = device.DetectedModuleType
			details["type_mismatch"] = device.TypeMismatch
			details["communication_module_version"] = device.CommunicationModuleVersion
			details["plc_version"] = device.PLCVersion
			details["version_state"] = device.VersionState
			if device.LastVersionError != "" {
				details["last_version_error"] = device.LastVersionError
			}
			return details
		}
	}
	return details
}

func (r *Runtime) invalidateStationVersionsForTopology(sourceID string, topology *domain.SourceTopology) {
	fingerprint := domain.SourceTopologyFingerprint(topology)
	r.mu.Lock()
	r.stations.removeOtherTopology(sourceID, fingerprint)
	r.mu.Unlock()
}

var _ api.DeviceVersionsService = (*Runtime)(nil)
