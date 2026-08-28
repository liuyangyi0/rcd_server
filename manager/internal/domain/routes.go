package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type RouteTable struct {
	configurationMu sync.RWMutex
	mu              sync.RWMutex
	groups          map[string]Group
	sources         map[string]Source
	sessions        map[string]*Session
	pendingByKey    map[string]PendingCommand
	pendingByID     map[string]string
	pendingByGroup  map[string]int
	nextCommandID   atomic.Uint64
	clock           func() time.Time
}

func NewRouteTable(groups []Group, sources []Source) (*RouteTable, error) {
	t := &RouteTable{
		groups:         make(map[string]Group),
		sources:        make(map[string]Source),
		sessions:       make(map[string]*Session),
		pendingByKey:   make(map[string]PendingCommand),
		pendingByID:    make(map[string]string),
		pendingByGroup: make(map[string]int),
		clock:          time.Now,
	}
	for _, group := range groups {
		if group.ID == "" {
			return nil, &FieldError{Field: "group.id", Message: "must not be empty"}
		}
		if _, ok := t.groups[group.ID]; ok {
			return nil, fmt.Errorf("group %q: %w", group.ID, ErrAlreadyExists)
		}
		if group.Revision == 0 {
			group.Revision = 1
		}
		t.groups[group.ID] = group
	}
	for _, source := range sources {
		if source.ID == "" {
			return nil, &FieldError{Field: "source.id", Message: "must not be empty"}
		}
		if _, ok := t.groups[source.GroupID]; !ok {
			return nil, fmt.Errorf("source %q group %q: %w", source.ID, source.GroupID, ErrNotFound)
		}
		if _, ok := t.sources[source.ID]; ok {
			return nil, fmt.Errorf("source %q: %w", source.ID, ErrAlreadyExists)
		}
		t.sources[source.ID] = source
	}
	for _, group := range t.groups {
		if group.DefaultSourceID == "" {
			continue
		}
		if err := t.validateSourceMembershipLocked(group.ID, group.DefaultSourceID); err != nil {
			return nil, fmt.Errorf("default source for group %q: %w", group.ID, err)
		}
	}
	return t, nil
}

// OpenSession is idempotent. Re-attaching the same OPC UA Session keeps its
// routes, while a genuinely new Session copies current group defaults.
func (t *RouteTable) OpenSession(id string) (Session, error) {
	if id == "" {
		return Session{}, &FieldError{Field: "session_id", Message: "must not be empty"}
	}
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock().UTC()
	if existing := t.sessions[id]; existing != nil {
		existing.LastSeenAt = now
		return cloneSession(*existing), nil
	}
	s := &Session{ID: id, ActivatedAt: now, LastSeenAt: now, Routes: make(map[string]GroupRoute)}
	for groupID, group := range t.groups {
		sourceID := group.DefaultSourceID
		if sourceID != "" {
			if err := t.validateSourceLocked(groupID, sourceID); err != nil {
				sourceID = ""
			}
		}
		s.Routes[groupID] = GroupRoute{GroupID: groupID, SourceID: sourceID, Revision: 1}
	}
	t.sessions[id] = s
	return cloneSession(*s), nil
}

func (t *RouteTable) TouchSession(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[id]
	if s == nil {
		return ErrSessionClosed
	}
	s.LastSeenAt = t.clock().UTC()
	return nil
}

func (t *RouteTable) CloseSession(id string) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, id)
}

func (t *RouteTable) Session(id string) (Session, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.sessions[id]
	if s == nil {
		return Session{}, ErrSessionClosed
	}
	return cloneSession(*s), nil
}

// Route returns one immutable route without allocating a full Session
// snapshot. OPC sampling calls this for every monitored node.
func (t *RouteTable) Route(sessionID, groupID string) (GroupRoute, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	session := t.sessions[sessionID]
	if session == nil {
		return GroupRoute{}, ErrSessionClosed
	}
	route, ok := session.Routes[groupID]
	if !ok {
		return GroupRoute{}, ErrNotFound
	}
	return route, nil
}

func (t *RouteTable) Sessions() []Session {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Session, 0, len(t.sessions))
	for _, session := range t.sessions {
		out = append(out, cloneSession(*session))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *RouteTable) Groups() []Group {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Group, 0, len(t.groups))
	for _, group := range t.groups {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *RouteTable) Sources() []Source {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Source, 0, len(t.sources))
	for _, source := range t.sources {
		out = append(out, source)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ApplyConfiguration atomically replaces group/source configuration without
// rebuilding the table or losing existing Session selections. Existing Session
// routes never follow a default-source change. Newly added groups are appended
// to every live Session using the new group's current default.
func (t *RouteTable) ApplyConfiguration(groups []Group, sources []Source) error {
	candidate, err := NewRouteTable(groups, sources)
	if err != nil {
		return err
	}
	t.configurationMu.Lock()
	defer t.configurationMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.validateConfigurationTransitionLocked(candidate); err != nil {
		return err
	}
	t.applyConfigurationLocked(candidate)
	return nil
}

// CommitConfiguration validates and serializes route-changing operations while
// persist commits, but does not hold the hot route data lock during database or
// DPAPI work. Reads therefore continue against the old immutable configuration
// until a successful commit is published. A failed write cannot leak a partial
// group/source or invalidate a Session route. The caller may replace elements
// in the supplied slices with repository-returned metadata before persist
// returns; topology and catalog state always come from the validated candidate.
func (t *RouteTable) CommitConfiguration(groups []Group, sources []Source, persist func() error) error {
	candidate, err := NewRouteTable(groups, sources)
	if err != nil {
		return err
	}
	t.configurationMu.Lock()
	defer t.configurationMu.Unlock()
	t.mu.Lock()
	if err := t.validateConfigurationTransitionLocked(candidate); err != nil {
		t.mu.Unlock()
		return err
	}
	t.mu.Unlock()
	if persist != nil {
		if err := persist(); err != nil {
			return err
		}
		mergePersistedConfigurationMetadata(candidate, groups, sources)
	}
	t.mu.Lock()
	t.applyConfigurationLocked(candidate)
	t.mu.Unlock()
	return nil
}

func mergePersistedConfigurationMetadata(candidate *RouteTable, groups []Group, sources []Source) {
	for _, persisted := range groups {
		group, ok := candidate.groups[persisted.ID]
		if !ok {
			continue
		}
		group.Revision = persisted.Revision
		group.CreatedAt = persisted.CreatedAt
		group.UpdatedAt = persisted.UpdatedAt
		candidate.groups[persisted.ID] = group
	}
	for _, persisted := range sources {
		source, ok := candidate.sources[persisted.ID]
		if !ok {
			continue
		}
		source.Revision = persisted.Revision
		source.CreatedAt = persisted.CreatedAt
		source.UpdatedAt = persisted.UpdatedAt
		source.HasPassword = persisted.HasPassword
		candidate.sources[persisted.ID] = source
	}
}

func (t *RouteTable) validateConfigurationTransitionLocked(candidate *RouteTable) error {
	for groupID := range t.groups {
		candidateGroup, retained := candidate.groups[groupID]
		if retained {
			if t.pendingByGroup[groupID] > 0 && candidateGroup.DefaultSourceID != t.groups[groupID].DefaultSourceID {
				return ErrGroupPending
			}
			continue
		}
		if t.pendingByGroup[groupID] > 0 {
			return ErrGroupPending
		}
		if len(t.sessions) > 0 {
			return fmt.Errorf("group %q: %w", groupID, ErrGroupInUse)
		}
	}
	for sourceID, oldSource := range t.sources {
		newSource, retained := candidate.sources[sourceID]
		if retained && newSource.GroupID == oldSource.GroupID {
			continue
		}
		if _, groupRemoved := candidate.groups[oldSource.GroupID]; !groupRemoved {
			// The source is removed as part of an already validated whole-group
			// removal, so its former default relationship is not independently in use.
			continue
		}
		if t.sourceInUseLocked(sourceID) {
			return fmt.Errorf("source %q: %w", sourceID, ErrSourceInUse)
		}
	}
	return nil
}

func (t *RouteTable) applyConfigurationLocked(candidate *RouteTable) {
	t.groups = candidate.groups
	t.sources = candidate.sources
	for _, session := range t.sessions {
		for groupID := range session.Routes {
			if _, exists := t.groups[groupID]; !exists {
				delete(session.Routes, groupID)
			}
		}
		for groupID, group := range t.groups {
			if _, exists := session.Routes[groupID]; !exists {
				sourceID := group.DefaultSourceID
				if sourceID != "" {
					if err := t.validateSourceLocked(groupID, sourceID); err != nil {
						sourceID = ""
					}
				}
				session.Routes[groupID] = GroupRoute{GroupID: groupID, SourceID: sourceID, Revision: 1}
			}
		}
		for groupID, route := range session.Routes {
			if route.SourceID == "" {
				continue
			}
			if err := t.validateSourceLocked(groupID, route.SourceID); err != nil {
				// A command pins its originating route until its terminal ACK or
				// timeout. Catalog state may change concurrently, but must not
				// rewrite any Session selection while the group lock is active.
				if t.pendingByGroup[groupID] > 0 {
					continue
				}
				route.SourceID = ""
				route.Revision++
				session.Routes[groupID] = route
			}
		}
	}
}

func (t *RouteTable) Select(sessionID, groupID, sourceID string, expectedRevision uint64) (GroupRoute, error) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingByGroup[groupID] > 0 {
		return GroupRoute{}, ErrGroupPending
	}
	s := t.sessions[sessionID]
	if s == nil {
		return GroupRoute{}, ErrSessionClosed
	}
	route, ok := s.Routes[groupID]
	if !ok {
		return GroupRoute{}, ErrNotFound
	}
	if route.Revision != expectedRevision {
		return GroupRoute{}, &RevisionConflictError{Expected: expectedRevision, Actual: route.Revision}
	}
	if err := t.validateSourceLocked(groupID, sourceID); err != nil {
		return GroupRoute{}, err
	}
	route.SourceID = sourceID
	route.Revision++
	s.Routes[groupID] = route
	s.LastSeenAt = t.clock().UTC()
	return route, nil
}

func (t *RouteTable) SetDefault(groupID, sourceID string, expectedRevision uint64) (Group, error) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pendingByGroup[groupID] > 0 {
		return Group{}, ErrGroupPending
	}
	group, ok := t.groups[groupID]
	if !ok {
		return Group{}, ErrNotFound
	}
	if group.Revision != expectedRevision {
		return Group{}, &RevisionConflictError{Expected: expectedRevision, Actual: group.Revision}
	}
	if err := t.validateSourceLocked(groupID, sourceID); err != nil {
		return Group{}, err
	}
	group.DefaultSourceID = sourceID
	group.Revision++
	group.UpdatedAt = t.clock().UTC()
	t.groups[groupID] = group
	return group, nil
}

func (t *RouteTable) BeginCommand(sessionID, groupID, port, kind string) (PendingCommand, error) {
	if port == "" {
		return PendingCommand{}, &FieldError{Field: "port", Message: "must not be empty"}
	}
	if kind == "" {
		return PendingCommand{}, &FieldError{Field: "kind", Message: "must not be empty"}
	}
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[sessionID]
	if s == nil {
		return PendingCommand{}, ErrSessionClosed
	}
	route, ok := s.Routes[groupID]
	if !ok || route.SourceID == "" {
		return PendingCommand{}, ErrNotFound
	}
	if err := t.validateSourceLocked(groupID, route.SourceID); err != nil {
		return PendingCommand{}, err
	}
	key := route.SourceID + "\x00" + port
	if _, exists := t.pendingByKey[key]; exists {
		return PendingCommand{}, ErrCommandPending
	}
	id := t.newCommandIDLocked()
	command := PendingCommand{
		ID: id, SessionID: sessionID, GroupID: groupID, SourceID: route.SourceID,
		Port: port, Kind: kind, State: CommandWaiting,
		StatusCode: StatusBadWaitingForResponse, StartedAt: t.clock().UTC(),
	}
	t.pendingByKey[key] = command
	t.pendingByID[id] = key
	t.pendingByGroup[groupID]++
	return command, nil
}

// RestorePending rehydrates a persisted in-flight command after a process
// restart. It intentionally does not require the originating Session to exist;
// the pinned source/port still has to reach ACK or timeout before selection is
// unlocked.
func (t *RouteTable) RestorePending(command PendingCommand) error {
	if command.ID == "" || command.Port == "" || command.GroupID == "" || command.SourceID == "" {
		return &FieldError{Field: "pending_command", Message: "id, group_id, source_id, and port are required"}
	}
	if command.State != CommandWaiting {
		return &FieldError{Field: "state", Message: "only Waiting commands can be restored"}
	}
	if !command.FinishedAt.IsZero() {
		return &FieldError{Field: "finished_at", Message: "a Waiting command cannot already be finished"}
	}
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.validateSourceMembershipLocked(command.GroupID, command.SourceID); err != nil {
		return err
	}
	if _, exists := t.pendingByID[command.ID]; exists {
		return ErrAlreadyExists
	}
	key := command.SourceID + "\x00" + command.Port
	if _, exists := t.pendingByKey[key]; exists {
		return ErrCommandPending
	}
	if command.StartedAt.IsZero() {
		command.StartedAt = t.clock().UTC()
	}
	command.StatusCode = StatusBadWaitingForResponse
	t.pendingByKey[key] = command
	t.pendingByID[command.ID] = key
	t.pendingByGroup[command.GroupID]++
	return nil
}

func (t *RouteTable) FinishCommand(id string, state CommandState, status StatusCode, message string) (PendingCommand, error) {
	if !terminalCommandState(state) {
		return PendingCommand{}, &FieldError{Field: "state", Message: "must be Received, Failed, or TimedOut"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key, ok := t.pendingByID[id]
	if !ok {
		return PendingCommand{}, ErrNotFound
	}
	return t.finishCommandLocked(key, state, status, message, t.clock().UTC()), nil
}

// PrepareCommandFinish creates the terminal audit record without releasing the
// in-memory command lock. Runtime persists this record before CommitCommandFinish
// so a database error cannot resurrect a stale Waiting command after restart.
func (t *RouteTable) PrepareCommandFinish(id string, state CommandState, status StatusCode, message string, finishedAt time.Time) (PendingCommand, error) {
	if !terminalCommandState(state) {
		return PendingCommand{}, &FieldError{Field: "state", Message: "must be Received, Failed, or TimedOut"}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	key, ok := t.pendingByID[id]
	if !ok {
		return PendingCommand{}, ErrNotFound
	}
	command := t.pendingByKey[key]
	command.State = state
	command.StatusCode = status
	command.Error = message
	command.FinishedAt = finishedAt.UTC()
	return command, nil
}

// CommitCommandFinish releases a command only after its terminal audit row has
// been durably written.
func (t *RouteTable) CommitCommandFinish(id string) ([]SessionRouteChange, error) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	key, ok := t.pendingByID[id]
	if !ok {
		return nil, ErrNotFound
	}
	command := t.pendingByKey[key]
	delete(t.pendingByID, id)
	delete(t.pendingByKey, key)
	t.pendingByGroup[command.GroupID]--
	if t.pendingByGroup[command.GroupID] == 0 {
		delete(t.pendingByGroup, command.GroupID)
		return t.invalidateUnselectableRoutesLocked(command.GroupID), nil
	}
	return nil, nil
}

func (t *RouteTable) invalidateUnselectableRoutesLocked(groupID string) []SessionRouteChange {
	changes := make([]SessionRouteChange, 0)
	for sessionID, session := range t.sessions {
		route, exists := session.Routes[groupID]
		if !exists || route.SourceID == "" || t.validateSourceLocked(groupID, route.SourceID) == nil {
			continue
		}
		route.SourceID = ""
		route.Revision++
		session.Routes[groupID] = route
		changes = append(changes, SessionRouteChange{SessionID: sessionID, Route: route})
	}
	return changes
}

// ExpiredPending returns immutable candidates without releasing their locks.
func (t *RouteTable) ExpiredPending(now time.Time, maxAge time.Duration) []PendingCommand {
	if maxAge <= 0 {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	cutoff := now.Add(-maxAge)
	result := make([]PendingCommand, 0)
	for _, command := range t.pendingByKey {
		if !command.StartedAt.After(cutoff) {
			result = append(result, command)
		}
	}
	return result
}

func (t *RouteTable) PendingCommand(sourceID, port string) (PendingCommand, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	command, ok := t.pendingByKey[sourceID+"\x00"+port]
	return command, ok
}

func (t *RouteTable) FinishCommandBySourcePort(sourceID, port string, state CommandState, status StatusCode, message string) (PendingCommand, error) {
	if !terminalCommandState(state) {
		return PendingCommand{}, &FieldError{Field: "state", Message: "must be Received, Failed, or TimedOut"}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := sourceID + "\x00" + port
	if _, ok := t.pendingByKey[key]; !ok {
		return PendingCommand{}, ErrNotFound
	}
	return t.finishCommandLocked(key, state, status, message, t.clock().UTC()), nil
}

// SweepExpired releases command locks whose deadlines are older than maxAge.
// The returned terminal commands can be persisted to the audit repository.
func (t *RouteTable) SweepExpired(now time.Time, maxAge time.Duration) []PendingCommand {
	if maxAge <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-maxAge)
	keys := make([]string, 0)
	for key, command := range t.pendingByKey {
		if !command.StartedAt.After(cutoff) {
			keys = append(keys, key)
		}
	}
	expired := make([]PendingCommand, 0, len(keys))
	for _, key := range keys {
		expired = append(expired, t.finishCommandLocked(key, CommandTimedOut, StatusBadTimeout, "command timed out", now.UTC()))
	}
	return expired
}

func (t *RouteTable) finishCommandLocked(key string, state CommandState, status StatusCode, message string, finishedAt time.Time) PendingCommand {
	command := t.pendingByKey[key]
	delete(t.pendingByID, command.ID)
	delete(t.pendingByKey, key)
	t.pendingByGroup[command.GroupID]--
	if t.pendingByGroup[command.GroupID] == 0 {
		delete(t.pendingByGroup, command.GroupID)
	}
	command.State = state
	command.StatusCode = status
	command.Error = message
	command.FinishedAt = finishedAt
	return command
}

func (t *RouteTable) PendingCount(groupID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pendingByGroup[groupID]
}

func (t *RouteTable) validateSourceLocked(groupID, sourceID string) error {
	if err := t.validateSourceMembershipLocked(groupID, sourceID); err != nil {
		return err
	}
	source := t.sources[sourceID]
	if !source.Enabled || !source.CatalogState.Selectable() {
		return ErrSourceNotSelectable
	}
	return nil
}

func (t *RouteTable) validateSourceMembershipLocked(groupID, sourceID string) error {
	source, ok := t.sources[sourceID]
	if !ok {
		return ErrNotFound
	}
	if source.GroupID != groupID {
		return ErrSourceNotInGroup
	}
	return nil
}

func (t *RouteTable) sourceInUseLocked(sourceID string) bool {
	for _, group := range t.groups {
		if group.DefaultSourceID == sourceID {
			return true
		}
	}
	for _, session := range t.sessions {
		for _, route := range session.Routes {
			if route.SourceID == sourceID {
				return true
			}
		}
	}
	for _, command := range t.pendingByKey {
		if command.SourceID == sourceID {
			return true
		}
	}
	return false
}

func (t *RouteTable) newCommandIDLocked() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		raw[6] = (raw[6] & 0x0f) | 0x40
		raw[8] = (raw[8] & 0x3f) | 0x80
		encoded := hex.EncodeToString(raw[:])
		return "cmd-" + encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	}
	return fmt.Sprintf("cmd-%d-%d", t.clock().UTC().UnixNano(), t.nextCommandID.Add(1))
}

func terminalCommandState(state CommandState) bool {
	return state == CommandReceived || state == CommandFailed || state == CommandTimedOut
}

func cloneSession(session Session) Session {
	clone := session
	clone.Routes = make(map[string]GroupRoute, len(session.Routes))
	for groupID, route := range session.Routes {
		clone.Routes[groupID] = route
	}
	return clone
}
