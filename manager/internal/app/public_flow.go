package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// PublicFlow performs no network requests, database queries or catalog scans of
// business values. Topologies and station entries are immutable pinned views.
func (r *Runtime) PublicFlow(ctx context.Context) (api.FlowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return api.FlowSnapshot{}, err
	}
	now := time.Now().UTC()
	r.configMu.RLock()
	sources, groups := r.routes.Sources(), r.routes.Groups()
	r.mu.RLock()
	statuses := cloneStatuses(r.statuses)
	stationView := r.stations.view()
	topologies := make(map[string]*domain.SourceTopology, len(sources))
	counts := make(map[string]uint64, len(sources))
	unsupported := make(map[string]bool, len(sources))
	for _, source := range sources {
		topologies[source.ID] = r.catalogs[source.ID].Topology
		counts[source.ID] = r.flowSamples[source.ID]
		known, supported := r.stationSync.support(source.ID, statuses[source.ID].Epoch)
		unsupported[source.ID] = known && !supported
	}
	started := r.startedAt
	r.mu.RUnlock()
	r.configMu.RUnlock()
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	out := api.FlowSnapshot{GeneratedAt: now, Instance: started.Format(time.RFC3339Nano), Endpoint: publicFlowAddress(r.options.OPCEndpoint), Running: r.running.Load(), PendingCommands: r.routes.TotalPending(), Sources: []api.FlowSource{}, Groups: []api.FlowGroup{}, Sessions: []api.FlowSession{}}
	for _, source := range sources {
		status := statuses[source.ID]
		online := source.Enabled && status.Connection == opc.ConnectionConnected && status.Session == opc.SessionActive
		view := api.FlowSource{ID: source.ID, Name: flowText(source.Name), Address: publicFlowAddress(source.Endpoint), Enabled: source.Enabled, Connected: online, Subscribed: online && status.Subscription == opc.SubscriptionActive, CatalogState: string(source.CatalogState), LastDataAt: status.LastDataAt, Samples: strconv.FormatUint(counts[source.ID], 10), Ports: []api.FlowPort{}}
		entry := stationView.entries[source.ID]
		topology := topologies[source.ID]
		fingerprint := domain.SourceTopologyFingerprint(topology)
		devices := 0
		if topology != nil {
			for _, port := range topology.Ports {
				if len(view.Ports) >= 64 {
					view.Truncated = true
					break
				}
				pv := api.FlowPort{ID: port.PortID, Name: flowText(port.PortName), Devices: []api.FlowDevice{}}
				for _, device := range port.Devices {
					if devices >= 512 {
						view.Truncated = true
						break
					}
					devices++
					var ps domain.StationVersionPort
					var ds domain.StationVersion
					var observed time.Time
					fresh := false
					if entry != nil {
						observed = entry.snapshot.ObservedAt
						if loc, exists := entry.devices[domain.TopologyOwner{PortID: port.PortID, DeviceID: device.DeviceID}]; exists {
							ps, ds = entry.snapshot.Ports[loc.port], entry.snapshot.Ports[loc.port].Devices[loc.device]
							fresh = entry.live && entry.epoch == status.Epoch && !entry.snapshot.Cached && entry.snapshot.TopologyFingerprint == fingerprint && !unsupported[source.ID]
						}
					}
					state := domain.ResolveStationCommunication(online, fresh, observed, now, ps, ds)
					pv.Devices = append(pv.Devices, api.FlowDevice{ID: device.DeviceID, Name: flowText(device.Name), GroupID: source.GroupForDevice(port.PortID, device.DeviceID), State: state.State, ObservedAt: state.ObservedAt, PointCount: len(device.NodeIDs)})
				}
				sort.Slice(pv.Devices, func(i, j int) bool { return pv.Devices[i].ID < pv.Devices[j].ID })
				view.Ports = append(view.Ports, pv)
			}
		}
		sort.Slice(view.Ports, func(i, j int) bool { return view.Ports[i].ID < view.Ports[j].ID })
		out.Sources = append(out.Sources, view)
	}
	for _, group := range groups {
		view := api.FlowGroup{ID: group.ID, Name: flowText(group.Name), DefaultSource: group.DefaultSourceID, Members: []string{}}
		for _, source := range sources {
			if source.HasGroup(group.ID) {
				view.Members = append(view.Members, source.ID)
			}
		}
		out.Groups = append(out.Groups, view)
	}
	sessions, err := r.ListSessions(ctx)
	if err != nil {
		return api.FlowSnapshot{}, err
	}
	for _, session := range sessions {
		digest := sha256.Sum256([]byte(out.Instance + "\x00" + session.ID))
		view := api.FlowSession{ID: hex.EncodeToString(digest[:8]), Name: flowText(session.ClientName), Attached: session.Attached, Routes: map[string]string{}, Subscriptions: session.Subscriptions, MonitoredItems: session.MonitoredItems, Notifications: session.Notifications}
		for group, route := range session.Routes {
			view.Routes[group] = route.SourceID
		}
		if view.Notifications == nil {
			view.Notifications = map[string]uint32{}
		}
		out.Sessions = append(out.Sessions, view)
	}
	if r.calculations != nil {
		status := r.calculations.status()
		out.Calculation = api.FlowCalculation{Active: status.ActiveRevision != 0, Rules: status.RuleCount, Good: status.GoodCount, Bad: status.BadCount, LastEvaluationAt: status.LastEvaluationAt, DurationMS: status.LastEvaluationDurationMS}
	}
	return out, nil
}

func publicFlowAddress(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return ""
	}
	// Do not disclose credentials, query strings or arbitrary endpoint paths.
	return u.Scheme + "://" + u.Host
}

func flowText(value string) string {
	runes := []rune(value)
	if len(runes) > 128 {
		runes = runes[:128]
	}
	return string(runes)
}

func (r *Runtime) PublicFlowPoints(ctx context.Context, request api.PageRequest) (api.FlowPointPage, error) {
	r.configMu.RLock()
	_, err := r.source(request.SourceID)
	r.mu.RLock()
	index := r.index
	r.mu.RUnlock()
	r.configMu.RUnlock()
	if err != nil || index == nil {
		return api.FlowPointPage{}, domain.ErrNotFound
	}
	out := api.FlowPointPage{Items: []api.FlowPoint{}, Offset: request.Offset}
	query := strings.ToLower(request.Query)
	for _, point := range index.points {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		node, exists := index.sourceNode(request.SourceID, point.Node.Key.Identifier)
		if !exists || node.Class != domain.NodeClassVariable || node.Kind == "command" || node.Writable || node.Retired || (request.PortID != "" && node.PortID != request.PortID) || (request.DeviceID != "" && node.DeviceID != request.DeviceID) || !strings.Contains(strings.ToLower(node.Key.Identifier), query) {
			continue
		}
		out.Total++
		if out.Total <= request.Offset || len(out.Items) >= 20 {
			continue
		}
		sample := r.sampleForSource(request.SourceID, point)
		value, marshalErr := json.Marshal(sample.Value)
		omitted := marshalErr != nil || len(value) > 2048
		if omitted {
			value = json.RawMessage("null")
		}
		out.Items = append(out.Items, api.FlowPoint{ID: node.Key.Identifier, Type: string(node.DataType), Value: value, Status: uint32(sample.StatusCode), SourceTimestamp: sample.SourceTimestamp, ValueOmitted: omitted})
	}
	return out, nil
}
