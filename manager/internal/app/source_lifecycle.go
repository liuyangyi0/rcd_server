package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
)

// Runtime coordinates callback fences with status/cache/catalog commits under
// r.mu; sourceConnections owns the handles. Calls that can start workers retain
// configMu so shutdown can close admission before waiting on the WaitGroup.
func (r *Runtime) startLower(source domain.Source) error {
	r.mu.Lock()
	ctx := r.runContext
	if ctx == nil || !r.running.Load() || ctx.Err() != nil {
		r.mu.Unlock()
		return nil
	}
	generation, cancelPrevious := r.connections.begin(source.ID)
	previousStatus := r.statuses[source.ID]
	r.mu.Unlock()
	cancelLower(cancelPrevious)
	attached := false
	defer func() {
		if !attached {
			r.mu.Lock()
			r.connections.finish(source.ID, generation)
			r.mu.Unlock()
		}
	}()

	_, password, err := r.repo.SourceCredential(ctx, source.ID)
	if err != nil {
		return err
	}
	proxy := lowerSinkProxy{runtime: r, sourceID: source.ID, generation: generation,
		reconnectBase: previousStatus.ReconnectCount, lastDataAt: previousStatus.LastDataAt, lastProbeAt: previousStatus.LastProbeAt}
	var gateway string
	binding, _, fleetErr := r.repo.FleetBinding(ctx, source.ID)
	if fleetErr == nil {
		if binding.Machine == nil || binding.Machine.GatewayInstanceID == "" {
			return errors.New("统一台账缺少已核实的下级身份，未建立 OPC 连接")
		}
		gateway = binding.Machine.GatewayInstanceID
	} else if !errors.Is(fleetErr, domain.ErrNotFound) {
		return fleetErr
	}
	client, err := r.options.LowerFactory(opc.LowerConfig{
		ExpectedGatewayID: gateway,
		Source:            source, Password: password, CertificateFile: r.options.CertificateFile,
		PrivateKeyFile: r.options.PrivateKeyFile, PublishingInterval: r.options.PublishingInterval,
		SamplingInterval: r.options.SamplingInterval, MonitorBatchSize: opc.DefaultMonitorBatchSize,
		OperationTimeout: r.options.CommandTimeout,
	}, proxy)
	if err != nil {
		return err
	}
	if client == nil {
		return errors.New("lower OPC factory returned no client")
	}
	lowerCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if !r.running.Load() || ctx.Err() != nil || !r.connections.attach(source.ID, generation, client, cancel) {
		r.mu.Unlock()
		cancel()
		return nil
	}
	r.wg.Add(1)
	attached = true
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer cancel()
		runErr := client.Run(lowerCtx)
		// Retire the producer before reporting terminal status. Retain the last
		// global epoch: a synthetic local epoch 0 would be older than its last
		// connected status and silently discarded by onLowerStatus.
		r.mu.Lock()
		terminalGeneration, current := r.connections.finish(source.ID, generation)
		status := r.statuses[source.ID]
		r.mu.Unlock()
		if !current {
			return
		}
		if lowerCtx.Err() == nil {
			if runErr == nil {
				runErr = errors.New("lower OPC supervisor stopped unexpectedly")
			}
			slog.Error("lower OPC supervisor stopped", "source_id", source.ID, "error", runErr)
			status.LastError = runErr.Error()
		} else {
			status.LastError = lowerCtx.Err().Error()
		}
		status.SourceID, status.Connection = source.ID, opc.ConnectionDisconnected
		status.Session, status.Subscription = opc.SessionInactive, opc.SubscriptionInactive
		status.Catalog = "" // connection failure is not a new topology rejection
		status.UpdatedAt = time.Now().UTC()
		// This terminal-only fence is invalidated by any subsequent start/stop.
		r.onLowerStatus(status, terminalGeneration)
	}()
	return nil
}

func (r *Runtime) recordLowerStartFailure(source domain.Source, err error) {
	r.mu.RLock()
	current := r.statuses[source.ID]
	r.mu.RUnlock()
	r.OnLowerStatus(opc.LowerStatus{SourceID: source.ID, Connection: opc.ConnectionDisconnected,
		Session: opc.SessionInactive, Subscription: opc.SubscriptionInactive,
		Epoch: current.Epoch, ReconnectCount: current.ReconnectCount,
		LastDataAt: current.LastDataAt, LastProbeAt: current.LastProbeAt,
		LastError: err.Error(), UpdatedAt: time.Now().UTC()})
}

func (r *Runtime) stopLower(sourceID string) {
	r.mu.Lock()
	cancel := r.connections.stop(sourceID)
	previous, existed := r.statuses[sourceID]
	status := previous
	status.SourceID = sourceID
	status.Connection = opc.ConnectionDisconnected
	status.Session = opc.SessionInactive
	status.Subscription = opc.SubscriptionInactive
	status.UpdatedAt = time.Now().UTC()
	r.statuses[sourceID] = status
	r.mu.Unlock()
	cancelLower(cancel)
	r.cancelDisconnectedQueued()
	if existed && lowerStatusControlChanged(previous, status) {
		r.persistLowerStatusTransition(previous, status, true)
	}
}

type lowerSinkProxy struct {
	runtime       *Runtime
	sourceID      string
	generation    uint64
	reconnectBase uint64
	lastDataAt    time.Time
	lastProbeAt   time.Time
}

func (p lowerSinkProxy) OnLowerStatus(status opc.LowerStatus) {
	status.SourceID = p.sourceID
	status.Epoch = p.generation<<32 | status.Epoch&0xffffffff
	status.ReconnectCount += p.reconnectBase
	if status.LastDataAt.IsZero() {
		status.LastDataAt = p.lastDataAt
	}
	if status.LastProbeAt.IsZero() {
		status.LastProbeAt = p.lastProbeAt
	}
	p.runtime.onLowerStatus(status, p.generation)
}

func (p lowerSinkProxy) OnCatalog(catalog domain.SourceCatalog) {
	p.runtime.onCatalog(p.sourceID, p.generation, catalog)
}

func (p lowerSinkProxy) OnSample(_ string, key domain.NodeKey, sample domain.Sample) {
	sample.Epoch = p.generation<<32 | sample.Epoch&0xffffffff
	p.runtime.onSample(p.sourceID, key, sample, p.generation)
}

func (p lowerSinkProxy) OnStationVersionRevision(_ string, _ string, _ uint64) {
	p.runtime.mu.RLock()
	current := p.runtime.running.Load() && p.runtime.connections.current(p.sourceID, p.generation)
	p.runtime.mu.RUnlock()
	if current {
		p.runtime.queueStationVersionRefresh(p.sourceID)
	}
}

func (p lowerSinkProxy) OnCommandRevision(_ string, _ uint64) {
	p.runtime.mu.RLock()
	current := p.runtime.running.Load() && p.runtime.connections.current(p.sourceID, p.generation)
	p.runtime.mu.RUnlock()
	if current && p.runtime.tracking != nil {
		p.runtime.tracking.notify(p.sourceID)
	}
}
