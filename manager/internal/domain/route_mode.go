package domain

func (t *RouteTable) SetAutomatic(sessionID, groupID string, automatic bool, revision uint64) (GroupRoute, error) {
	t.configurationMu.RLock()
	defer t.configurationMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[sessionID]
	if s == nil {
		return GroupRoute{}, ErrSessionClosed
	}
	route, ok := s.Routes[groupID]
	if !ok {
		return GroupRoute{}, ErrNotFound
	}
	if route.Revision != revision {
		return GroupRoute{}, &RevisionConflictError{Expected: revision, Actual: route.Revision}
	}
	if t.pendingByGroup[groupID] > 0 {
		return GroupRoute{}, ErrGroupPending
	}
	route.Automatic = automatic
	route.Revision++
	s.Routes[groupID] = route
	return route, nil
}
