package app

import (
	"encoding/hex"
	"fmt"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"strings"
	"time"
	"unicode"
)

type commandRoute struct {
	DeviceRouted bool
	GroupID      string
	SourceID     string
	Port         string
	Key          domain.NodeKey
	SendKey      domain.NodeKey
}

type commandHistoryEntry struct {
	Route     commandRoute
	CommandID string
	Sample    domain.Sample
}

func commandAddress(payload string) (uint32, error) {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, payload)
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) == 0 {
		return 0, &domain.FieldError{Field: "HexData", Message: "must be an even-length hexadecimal frame with a device address"}
	}
	return uint32(decoded[0]), nil
}

func (r *Runtime) routingCatalogs() (map[string]domain.SourceCatalog, *catalogIndex) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Catalog values and their slices are immutable after publication. Only
	// the map is copied because quarantine changes one source's state in place.
	catalogs := make(map[string]domain.SourceCatalog, len(r.catalogs))
	for id, catalog := range r.catalogs {
		catalogs[id] = catalog
	}
	return catalogs, r.index
}

func (r *Runtime) resolveCommandRouteLocked(sessionID, identifier, payload, groupHint, kind string) (commandRoute, error) {
	session, err := r.routes.Session(sessionID)
	if err != nil {
		return commandRoute{}, err
	}
	sources := r.routes.Sources()
	catalogs, index := r.routingCatalogs()
	groupID := groupHint
	strict := groupHint != ""
	if groupHint == "" {
		publicPoint, exists := index.pointForNode(identifier)
		if !exists {
			return commandRoute{}, domain.ErrNodeNotConfigured
		}
		if publicPoint.Node.Retired {
			return commandRoute{}, domain.ErrNodeRetired
		}
		// The old public port may be named differently from the selected lower
		// port. Determine the logical equipment from any matching public port.
		for _, source := range sources {
			catalog := catalogs[source.ID]
			if len(source.GroupBindings) == 0 {
				continue
			}
			if catalog.Topology == nil {
				for _, node := range catalog.Nodes {
					if node.Key.Identifier == identifier {
						strict = true
					}
				}
				continue
			}
			for _, port := range catalog.Topology.Ports {
				if identifier == port.SendNodeID || identifier == port.SendMethodID {
					strict = true
				}
			}
		}
		if !strict {
			point, err := r.commandPointForSessionLocked(sessionID, identifier)
			if err != nil {
				return commandRoute{}, err
			}
			selected := session.Routes[point.GroupID].SourceID
			key := point.Node.Key
			port := strings.TrimSuffix(strings.TrimSuffix(identifier, "_SendCommand"), "_Send")
			return commandRoute{GroupID: point.GroupID, SourceID: selected, Port: port, Key: key, SendKey: domain.NodeKey{NamespaceURI: key.NamespaceURI, Identifier: port + "_Send"}}, nil
		}
	}
	address, err := commandAddress(payload)
	if err != nil {
		return commandRoute{}, err
	}
	if groupID == "" {
		groups := make(map[string]struct{})
		for _, source := range sources {
			catalog := catalogs[source.ID]
			if catalog.Topology == nil {
				if len(source.GroupBindings) > 0 {
					for _, node := range catalog.Nodes {
						if node.Key.Identifier == identifier {
							strict = true
						}
					}
				}
				continue
			}
			for _, port := range catalog.Topology.Ports {
				if identifier != port.SendNodeID && identifier != port.SendMethodID {
					continue
				}
				for _, device := range port.Devices {
					if device.DeviceAddress == address {
						groups[source.GroupForDevice(port.PortID, device.DeviceID)] = struct{}{}
					}
				}
			}
		}
		if len(groups) != 1 {
			return commandRoute{}, fmt.Errorf("public port and address do not identify one logical group: %w", domain.ErrNodeNotConfigured)
		}
		for group := range groups {
			groupID = group
		}
	}
	selected, exists := session.Routes[groupID]
	if !exists || selected.SourceID == "" {
		return commandRoute{}, domain.ErrNodeNotConfigured
	}
	source, exists := sourceByID(sources, selected.SourceID)
	if !exists || !source.HasGroup(groupID) || !source.Enabled || !source.CatalogState.Selectable() || !source.GroupAvailable(groupID) {
		return commandRoute{}, domain.ErrSourceNotSelectable
	}
	catalog := catalogs[source.ID]
	if catalog.Topology == nil {
		return commandRoute{}, domain.ErrNodeNotConfigured
	}
	var candidates []commandRoute
	for _, port := range catalog.Topology.Ports {
		for _, device := range port.Devices {
			if device.DeviceAddress != address || source.GroupForDevice(port.PortID, device.DeviceID) != groupID {
				continue
			}
			keyID := port.SendNodeID
			if kind == "Call" {
				keyID = port.SendMethodID
			}
			node, ok := index.sourceNode(source.ID, keyID)
			if !ok {
				return commandRoute{}, domain.ErrNodeNotConfigured
			}
			if node.Retired {
				return commandRoute{}, domain.ErrNodeRetired
			}
			candidates = append(candidates, commandRoute{DeviceRouted: true, GroupID: groupID, SourceID: source.ID, Port: strings.TrimSuffix(port.SendNodeID, "_Send"), Key: node.Key, SendKey: domain.NodeKey{NamespaceURI: node.Key.NamespaceURI, Identifier: port.SendNodeID}})
		}
	}
	if len(candidates) != 1 {
		return commandRoute{}, fmt.Errorf("selected source has ambiguous or missing device address in group: %w", domain.ErrNodeNotConfigured)
	}
	return candidates[0], nil
}

func historyKey(sessionID, identifier string) string { return sessionID + "\x00" + identifier }

func (r *Runtime) rememberCommandRoute(sessionID, identifier string, route commandRoute, command domain.PendingCommand, epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.routes.Route(sessionID, route.GroupID); err != nil {
		return
	}
	if r.commandHistory == nil {
		r.commandHistory = make(map[string]commandHistoryEntry)
	}
	readRoute := route
	readRoute.Key = route.SendKey
	entry := commandHistoryEntry{Route: readRoute, CommandID: command.ID, Sample: domain.Sample{Type: domain.DataTypeString, Value: "2", StatusCode: domain.StatusGood, SourceTimestamp: command.StartedAt, Epoch: epoch}}
	if identifier != "" {
		if strings.HasSuffix(identifier, "_SendCommand") {
			identifier = strings.TrimSuffix(identifier, "_SendCommand") + "_Send"
		}
		r.commandHistory[historyKey(sessionID, identifier)] = entry
	}
	r.commandHistory[historyKey(sessionID, "group:"+route.GroupID)] = entry
}

func (r *Runtime) rememberPassiveCommand(sessionID, identifier, groupHint string, route commandRoute, payload string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.routes.Route(sessionID, route.GroupID); err != nil {
		return
	}
	if r.commandHistory == nil {
		r.commandHistory = make(map[string]commandHistoryEntry)
	}
	lookup := identifier
	if groupHint != "" {
		lookup = "group:" + groupHint
	}
	if route.SendKey.Identifier != "" {
		route.Key = route.SendKey
	}
	// A reset belongs only to the initiating Session and public control node.
	// Clearing the command ID also prevents a late terminal ACK from undoing it.
	r.commandHistory[historyKey(sessionID, lookup)] = commandHistoryEntry{Route: route, Sample: domain.Sample{Type: domain.DataTypeString, Value: payload, StatusCode: domain.StatusGood, SourceTimestamp: time.Now().UTC(), Epoch: r.statuses[route.SourceID].Epoch}}
}

func (r *Runtime) finishCommandHistory(command domain.PendingCommand) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.commandHistory {
		if entry.CommandID != command.ID {
			continue
		}
		entry.Sample.Value = "3"
		if command.State == domain.CommandReceived {
			entry.Sample.Value = "1"
		}
		entry.Sample.StatusCode = command.StatusCode
		entry.Sample.SourceTimestamp = command.FinishedAt
		r.commandHistory[key] = entry
	}
}

func (r *Runtime) commandHistorySample(sessionID, identifier string) (domain.Sample, bool) {
	r.mu.RLock()
	entry, exists := r.commandHistory[historyKey(sessionID, identifier)]
	if !exists {
		r.mu.RUnlock()
		return domain.Sample{}, false
	}
	status := r.statuses[entry.Route.SourceID]
	node, nodeExists := r.index.sourceNode(entry.Route.SourceID, entry.Route.Key.Identifier)
	r.mu.RUnlock()
	if _, err := r.routes.Route(sessionID, entry.Route.GroupID); err != nil {
		return badCommandSample(), true
	}
	// This is the last result of this exact Session/node command, not the
	// physical port's latest ACK (which may belong to another equipment).
	sample := entry.Sample
	if sample.StatusCode.IsGood() {
		if !nodeExists || status.Catalog == domain.CatalogConflict {
			sample.StatusCode = domain.StatusBadConfigurationError
		} else if node.Retired {
			sample.StatusCode = domain.StatusBadOutOfService
		} else if status.Connection != opc.ConnectionConnected || status.Session != opc.SessionActive {
			sample.StatusCode = domain.StatusBadNoCommunication
		}
	}
	return sample, true
}

// resolveReadCommandRoute rejects a shared port whose device selections split
// across physical sources. A recent command is the only unambiguous exception.
func (r *Runtime) resolveReadCommandRoute(sessionID, identifier, groupHint string) (commandRoute, bool, error) {
	lookup := identifier
	if groupHint != "" {
		lookup = "group:" + groupHint
	}
	r.mu.RLock()
	history, hasHistory := r.commandHistory[historyKey(sessionID, lookup)]
	r.mu.RUnlock()
	sources := r.routes.Sources()
	catalogs, index := r.routingCatalogs()
	if hasHistory {
		selected, err := r.routes.Route(sessionID, history.Route.GroupID)
		if err != nil {
			return commandRoute{}, true, err
		}
		if selected.SourceID == history.Route.SourceID {
			source, exists := sourceByID(sources, selected.SourceID)
			if !exists || !source.Enabled || !source.CatalogState.Selectable() || !source.GroupAvailable(history.Route.GroupID) {
				return commandRoute{}, true, domain.ErrSourceNotSelectable
			}
			node, exists := index.sourceNode(history.Route.SourceID, history.Route.Key.Identifier)
			if !exists {
				return commandRoute{}, true, domain.ErrNodeNotConfigured
			}
			if node.Retired {
				return commandRoute{}, true, domain.ErrNodeRetired
			}
			return history.Route, true, nil
		}
	}
	groupIDs := make(map[string]struct{})
	strict := groupHint != ""
	if groupHint != "" {
		groupIDs[groupHint] = struct{}{}
	} else {
		for _, source := range sources {
			catalog := catalogs[source.ID]
			if catalog.Topology == nil {
				if len(source.GroupBindings) > 0 {
					for _, node := range catalog.Nodes {
						if node.Key.Identifier == identifier {
							strict = true
						}
					}
				}
				continue
			}
			for _, port := range catalog.Topology.Ports {
				if port.SendNodeID != identifier {
					continue
				}
				if len(source.GroupBindings) > 0 {
					strict = true
				}
				for _, device := range port.Devices {
					groupIDs[source.GroupForDevice(port.PortID, device.DeviceID)] = struct{}{}
				}
			}
		}
	}
	if !strict {
		return commandRoute{}, false, nil
	}
	var target commandRoute
	seen := make(map[string]struct{})
	for groupID := range groupIDs {
		selected, err := r.routes.Route(sessionID, groupID)
		if err != nil {
			return commandRoute{}, true, err
		}
		if selected.SourceID == "" {
			return commandRoute{}, true, domain.ErrNodeNotConfigured
		}
		source, exists := sourceByID(sources, selected.SourceID)
		if !exists || !source.Enabled || !source.CatalogState.Selectable() || !source.GroupAvailable(groupID) {
			return commandRoute{}, true, domain.ErrSourceNotSelectable
		}
		catalog := catalogs[source.ID]
		if catalog.Topology == nil {
			return commandRoute{}, true, domain.ErrNodeNotConfigured
		}
		for _, port := range catalog.Topology.Ports {
			for _, device := range port.Devices {
				if source.GroupForDevice(port.PortID, device.DeviceID) != groupID {
					continue
				}
				node, ok := index.sourceNode(source.ID, port.SendNodeID)
				if !ok {
					return commandRoute{}, true, domain.ErrNodeNotConfigured
				}
				if node.Retired {
					return commandRoute{}, true, domain.ErrNodeRetired
				}
				target = commandRoute{GroupID: groupID, SourceID: source.ID, Port: strings.TrimSuffix(port.SendNodeID, "_Send"), Key: node.Key, SendKey: node.Key}
				seen[source.ID+"\x00"+port.PortID] = struct{}{}
			}
		}
	}
	if len(seen) != 1 {
		return commandRoute{}, true, domain.ErrNodeNotConfigured
	}
	return target, true, nil
}

func badCommandSample() domain.Sample {
	return domain.Sample{Type: domain.DataTypeString, Value: "", StatusCode: domain.StatusBadConfigurationError}
}

func (r *Runtime) sampleCommandRoute(route commandRoute) domain.Sample {
	r.mu.RLock()
	index := r.index
	node, exists := index.sourceNode(route.SourceID, route.Key.Identifier)
	r.mu.RUnlock()
	if !exists {
		return badCommandSample()
	}
	return r.sampleForSourceAtIndex(route.SourceID, indexedPoint{GroupID: route.GroupID, Node: node}, index)
}

func (r *Runtime) readLegacyCommand(sessionID, identifier string) (domain.Sample, bool) {
	if sample, exists := r.commandHistorySample(sessionID, identifier); exists {
		return sample, true
	}
	route, handled, err := r.resolveReadCommandRoute(sessionID, identifier, "")
	if !handled {
		return domain.Sample{}, false
	}
	if err != nil {
		return badCommandSample(), true
	}
	return r.sampleCommandRoute(route), true
}

func (r *Runtime) ReadGroupCommand(sessionID, groupID string) domain.Sample {
	if sample, exists := r.commandHistorySample(sessionID, "group:"+groupID); exists {
		return sample
	}
	route, _, err := r.resolveReadCommandRoute(sessionID, "", groupID)
	if err != nil {
		return badCommandSample()
	}
	return r.sampleCommandRoute(route)
}
