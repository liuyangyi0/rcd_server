package domain

import "fmt"

// CommandQueueCapacity includes the executing command. Rejected writes are
// never silently overwritten or scheduled for retry.
const CommandQueueCapacity = 64

// Passive resets share the physical-port reservation but are not hardware
// commands and must never become a queue head or have commands appended to them.
const CommandKindPassiveReset = "PassiveReset"

// HasPortCommands covers both accepted queue entries and the executing
// transaction. Maintenance must not enter the gap before a queue head is
// claimed, or between two entries from the same physical port.
func (t *RouteTable) HasPortCommands(source, port string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	key := source + "\x00" + port
	_, executing := t.pendingByKey[key]
	return executing || len(t.queuedByPort[key]) != 0
}

func (t *RouteTable) HasCommand(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, exists := t.pendingByID[id]
	return exists
}

func (t *RouteTable) IsQueued(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, exists := t.queuedByID[id]
	return exists
}

func (t *RouteTable) CommandQueueCounts(group string) (queued, executing uint32) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, c := range t.queuedByID {
		if c.GroupID == group {
			queued++
		}
	}
	for _, c := range t.pendingByKey {
		if c.GroupID == group {
			executing++
		}
	}
	return
}

// EnqueueWrite reserves the group and physical source/port at acceptance.
// The route table owns BOTH waiting and executing reservations.
func (t *RouteTable) EnqueueWrite(sessionID, groupID, port, nodeID, request string) (PendingCommand, error) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.sessions[sessionID]
	if session == nil {
		return PendingCommand{}, ErrSessionClosed
	}
	route, ok := session.Routes[groupID]
	if !ok || route.SourceID == "" || port == "" {
		return PendingCommand{}, ErrNotFound
	}
	if err := t.validateSourceLocked(groupID, route.SourceID); err != nil {
		return PendingCommand{}, err
	}
	key := route.SourceID + "\x00" + port
	count := len(t.queuedByPort[key])
	if active, busy := t.pendingByKey[key]; busy {
		if active.Kind == CommandKindPassiveReset {
			return PendingCommand{}, ErrCommandPending
		}
		count++
	}
	if count >= CommandQueueCapacity {
		return PendingCommand{}, fmt.Errorf("physical port command queue full (%d including executing): %w", CommandQueueCapacity, ErrCommandPending)
	}
	c := PendingCommand{ID: t.newCommandIDLocked(), SessionID: sessionID,
		GroupID: groupID, SourceID: route.SourceID, Port: port, Kind: "Write",
		NodeID: nodeID, Request: request, State: CommandQueued,
		StatusCode: StatusBadWaitingForResponse, StartedAt: t.clock().UTC()}
	t.queuedByID[c.ID] = c
	t.queuedByPort[key] = append(t.queuedByPort[key], c.ID)
	t.pendingByID[c.ID] = key
	t.pendingByGroup[groupID]++
	return c, nil
}

func (t *RouteTable) QueuedCommands() []PendingCommand {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]PendingCommand, 0, len(t.queuedByID))
	for _, command := range t.queuedByID {
		result = append(result, command)
	}
	return result
}

func (t *RouteTable) QueueHead(source, port string) (PendingCommand, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	key := source + "\x00" + port
	if _, busy := t.pendingByKey[key]; busy || len(t.queuedByPort[key]) == 0 {
		return PendingCommand{}, false
	}
	return t.queuedByID[t.queuedByPort[key][0]], true
}

// ActivateQueued is a move, not a new reservation or Session lookup. Closing
// the originating Session never reroutes an already accepted command.
func (t *RouteTable) ActivateQueued(id string) (PendingCommand, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, exists := t.queuedByID[id]
	if !exists {
		return PendingCommand{}, ErrNotFound
	}
	key := t.pendingByID[id]
	if _, busy := t.pendingByKey[key]; busy || t.queuedByPort[key][0] != id {
		return PendingCommand{}, ErrCommandPending
	}
	t.removeQueuedLocked(id)
	c.State = CommandWaiting
	t.pendingByKey[key], t.pendingByID[id] = c, key
	return c, nil
}

func (t *RouteTable) removeQueuedLocked(id string) {
	key := t.pendingByID[id]
	ids := t.queuedByPort[key]
	for i, queuedID := range ids {
		if queuedID == id {
			ids = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(t.queuedByPort, key)
	} else {
		t.queuedByPort[key] = ids
	}
	delete(t.queuedByID, id)
	delete(t.pendingByID, id)
}
