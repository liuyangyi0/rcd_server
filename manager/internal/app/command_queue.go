package app

import (
	"rcd_manager/internal/api"
)

// Only immutable routing and transport identity are retained. The payload is
// the accepted command.Request, never a later read of a writable OPC variable.
type queuedCommandDispatch struct {
	route  commandRoute
	target lowerCommandTarget
}

func (r *Runtime) startQueuedWrite(source, port string) {
	r.configMu.Lock()
	if r.closing.Load() {
		r.configMu.Unlock()
		return
	}
	command, dispatch, ok := r.commands.claimQueued(source, port)
	if !ok {
		r.configMu.Unlock()
		return
	}
	r.wg.Add(1)
	r.configMu.Unlock()
	go func() {
		defer r.wg.Done()
		_, _ = r.executeQueuedWrite(command, dispatch)
	}()
}

// Cancellation never recursively dispatches another entry from the same queue.
func (r *Runtime) cancelQueuedPort(source, port, reason string) {
	for _, queued := range r.routes.QueuedCommands() {
		if queued.SourceID != source || queued.Port != port {
			continue
		}
		command, changes, err := r.commands.cancelQueued(queued.ID, reason, r.finishCommandHistory)
		if err != nil {
			continue
		}
		r.tracking.audit.record(command)
		r.broker.Publish(api.Event{Type: "command.changed", Data: command})
		for _, change := range changes {
			r.queueSessionRefresh(change.SessionID, change.Route.GroupID)
		}
	}
}

func (r *Runtime) cancelDisconnectedQueued() {
	for _, command := range r.routes.QueuedCommands() {
		r.commands.mu.Lock()
		meta := r.commands.meta[command.ID]
		r.commands.mu.Unlock()
		if meta.queued == nil {
			continue
		}
		target, ok := r.captureLowerCommandTarget(command.SourceID, command.Port, true)
		if r.closing.Load() || !ok || target.globalEpoch != meta.queued.target.globalEpoch ||
			target.generation != meta.queued.target.generation {
			r.cancelQueuedPort(command.SourceID, command.Port, "queued command connection lost; not sent")
		}
	}
}
