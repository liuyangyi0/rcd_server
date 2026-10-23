package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Installed separately from business configuration. The token is a native
// Semaphore read-only account token and is never exposed through manager APIs.
type fleetConfig struct {
	SchemaVersion int               `json:"schema_version"`
	PlatformURL   string            `json:"platform_url"`
	ProjectID     int               `json:"project_id"`
	TokenFile     string            `json:"token_file"`
	SourceDevices map[string]string `json:"source_devices"`
}

func (c fleetConfig) key() string {
	return strings.TrimRight(c.PlatformURL, "/") + "/api/project/" + strconv.Itoa(c.ProjectID) + "/rcd/fleet"
}
func (c fleetConfig) page(device string) string {
	return strings.TrimRight(c.PlatformURL, "/") + "/project/" + strconv.Itoa(c.ProjectID) + "/rcd/devices?device=" + url.QueryEscape(device)
}

type fleetSync struct {
	mu     sync.Mutex
	status domain.FleetStatus
}

func readFleetConfig(path string) (fleetConfig, error) {
	var c fleetConfig
	raw, err := readSmallFile(path, 16384)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, fmt.Errorf("统一台账配置格式错误")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("统一台账配置含额外内容")
	}
	u, err := url.Parse(c.PlatformURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || c.SchemaVersion != 1 || c.ProjectID < 1 || !filepath.IsAbs(c.TokenFile) || len(c.SourceDevices) < 1 || len(c.SourceDevices) > 20 {
		return c, fmt.Errorf("统一台账配置无效")
	}
	seen := map[string]bool{}
	for source, device := range c.SourceDevices {
		if !domain.ValidOperationRevision(source) || !domain.ValidOperationRevision(device) || seen[device] {
			return c, fmt.Errorf("台账来源映射重复或无效")
		}
		seen[device] = true
	}
	return c, nil
}
func readSmallFile(path string, limit int64) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("配置文件不是有界普通文件")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("配置文件超限")
	}
	return raw, e
}
func fetchFleet(ctx context.Context, c fleetConfig) (domain.FleetSnapshot, error) {
	var result domain.FleetSnapshot
	token, e := readSmallFile(c.TokenFile, 8192)
	if e != nil {
		return result, fmt.Errorf("无法读取平台只读凭据")
	}
	key := strings.TrimSpace(string(token))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return result, fmt.Errorf("平台只读凭据无效")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := http.Client{Timeout: 8 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, e := http.NewRequestWithContext(ctx, "GET", c.key(), nil)
	if e != nil {
		return result, e
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, e := client.Do(req)
	if e != nil {
		return result, fmt.Errorf("发布平台不可达；继续使用最后有效绑定")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("发布平台台账读取失败（HTTP %d）；保留缓存", resp.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &result) != nil || result.SchemaVersion != 1 || len(result.Machines) > 100 {
		return result, fmt.Errorf("发布平台返回了无效台账")
	}
	seen := map[string]bool{}
	agents := map[string]bool{}
	gateways := map[string]bool{}
	for _, m := range result.Machines {
		if e = m.Validate(); e != nil {
			return result, e
		}
		if seen[m.ID] || agents[m.AgentInstanceID] || gateways[m.GatewayInstanceID] {
			return result, fmt.Errorf("平台台账有重复物理身份")
		}
		seen[m.ID] = true
		agents[m.AgentInstanceID] = true
		gateways[m.GatewayInstanceID] = true
	}
	return result, nil
}
func (r *Runtime) FleetStatus(ctx context.Context) (domain.FleetStatus, error) {
	r.fleet.mu.Lock()
	v := r.fleet.status
	v.Sources = append([]domain.FleetSourceStatus{}, v.Sources...)
	r.fleet.mu.Unlock()
	// This endpoint only refreshes already received observations, not remote I/O.
	if r.options.FleetConfigFile == "" {
		v.Enabled = false
		return v, nil
	}
	v.Enabled = true
	return v, nil
}
func (r *Runtime) fleetWorker() {
	defer r.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		r.syncFleet(r.runContext)
		select {
		case <-r.runContext.Done():
			return
		case <-ticker.C:
		}
	}
}
func (r *Runtime) syncFleet(ctx context.Context) {
	status := domain.FleetStatus{Enabled: true, LastAttemptAt: time.Now().UTC(), Sources: []domain.FleetSourceStatus{}}
	defer func() { r.fleet.mu.Lock(); r.fleet.status = status; r.fleet.mu.Unlock() }()
	cfg, err := readFleetConfig(r.options.FleetConfigFile)
	if err != nil {
		status.Error = "统一台账连接配置不可读或无效；保留已保存绑定"
		return
	}
	status.PlatformURL = strings.TrimRight(cfg.PlatformURL, "/") + "/project/" + strconv.Itoa(cfg.ProjectID) + "/rcd/devices"
	snapshot, at, loadErr := r.repo.LoadFleetSnapshot(ctx, cfg.key())
	if loadErr == nil {
		status.LastSyncAt = at
	}
	fresh, fetchErr := fetchFleet(ctx, cfg)
	if fetchErr == nil {
		at, err = r.repo.SaveFleetSnapshot(ctx, cfg.key(), fresh)
		if err == nil {
			snapshot = fresh
			status.LastSyncAt = at
		} else {
			status.Error = "台账缓存保存失败，未应用新绑定"
			fetchErr = err
		}
	} else {
		status.Error = fetchErr.Error()
	}
	if fetchErr != nil && loadErr != nil {
		snapshot = domain.FleetSnapshot{}
	}
	machines := map[string]domain.FleetMachine{}
	for _, m := range snapshot.Machines {
		machines[m.ID] = m
	}
	sources := []string{}
	for source := range cfg.SourceDevices {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		device := cfg.SourceDevices[source]
		row, _, e := r.repo.FleetBinding(ctx, source)
		if e != nil {
			row = domain.FleetSourceStatus{SourceID: source, DeviceID: device}
		}
		row.PlatformURL = cfg.page(device)
		machine, found := machines[device]
		if found && row.Machine != nil && row.Machine.AgentInstanceID == machine.AgentInstanceID && row.Machine.GatewayInstanceID == machine.GatewayInstanceID {
			copy := *row.Machine
			copy.Components = machine.Components
			copy.LastSuccessAt = machine.LastSuccessAt
			copy.ActiveOperationID = machine.ActiveOperationID
			row.Machine = &copy
		}
		if !found {
			row.Error = "平台台账尚未登记该设备，保留现有来源定义"
		} else if fetchErr == nil {
			// Cache-only operation on platform failure. Do not apply stale network edits.
			e = r.applyFleet(ctx, cfg.key(), source, machine)
			if e != nil {
				row.Error = e.Error()
			} else {
				row, _, e = r.repo.FleetBinding(ctx, source)
				row.PlatformURL = cfg.page(device)
				if e == nil {
					copy := machine
					row.Machine = &copy
				}
			}
		}
		status.Sources = append(status.Sources, row)
	}
}

func (r *Runtime) applyFleet(ctx context.Context, platform, sourceID string, machine domain.FleetMachine) error {
	if err := machine.Validate(); err != nil {
		return err
	}
	r.configMu.RLock()
	current, ok := sourceByID(r.routes.Sources(), sourceID)
	binding, err := r.repo.GetProcessBinding(ctx, sourceID)
	if !ok {
		err = domain.ErrNotFound
	}
	if err == nil {
		err = r.checkSourceOperationLocked(sourceID)
	}
	if err == nil && r.routes.SourcePendingCount(sourceID) != 0 {
		err = domain.ErrCommandPending
	}
	cached, owner, cacheErr := r.repo.FleetBinding(ctx, sourceID)
	r.configMu.RUnlock()
	if err != nil {
		return fmt.Errorf("来源不存在或正在执行操作，暂不应用台账：%w", err)
	}
	if cacheErr == nil {
		old := cached.Machine
		if owner != platform || old == nil || cached.DeviceID != machine.ID || old.AgentInstanceID != machine.AgentInstanceID || old.GatewayInstanceID != machine.GatewayInstanceID || old.ManagerInstanceID != machine.ManagerInstanceID {
			return fmt.Errorf("稳定身份变化，拒绝替换；需人工核对原绑定")
		}
		if machine.Revision < old.Revision {
			return fmt.Errorf("平台版本早于已应用版本，保留原绑定")
		}
		if machine.Revision == old.Revision && !machine.SameBinding(*old) {
			return fmt.Errorf("同一台账 revision 含不同绑定，拒绝更新")
		}
		if machine.Revision == old.Revision && machine.Name == current.Name && machine.OPCEndpoint == current.Endpoint && machine.AgentURL == binding.AgentURL && machine.AgentInstanceID == binding.InstanceID {
			return nil
		}
	} else if !errors.Is(cacheErr, domain.ErrNotFound) {
		return cacheErr
	}
	if binding.Configured() && binding.InstanceID != machine.AgentInstanceID {
		return fmt.Errorf("Agent 稳定身份与原来源不一致")
	}
	if machine.ActiveOperationID != "" {
		return fmt.Errorf("发布平台有进行中或待确认作业 %s，暂不改变地址", machine.ActiveOperationID)
	}
	candidateBinding := binding
	candidateBinding.AgentURL, candidateBinding.InstanceID = machine.AgentURL, machine.AgentInstanceID
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	snapshot, err := r.options.ProcessAgentClient.Status(readCtx, candidateBinding)
	if err != nil {
		return fmt.Errorf("目标 Agent 不可达，保留原绑定")
	}
	if err = snapshot.Validate(machine.AgentInstanceID); err != nil {
		return err
	}
	if !snapshot.LifecycleSupported || snapshot.GatewayInstanceID != machine.GatewayInstanceID {
		return fmt.Errorf("Agent 能力或下级身份与台账不符")
	}
	if snapshot.PendingRequestID != "" || snapshot.MaintenanceDeploymentID != "" || (snapshot.Lifecycle != nil && snapshot.Lifecycle.State != "succeeded" && snapshot.Lifecycle.State != "failed") {
		return fmt.Errorf("目标 Agent 正在维护或结果未知，暂不改变绑定")
	}
	// An address change requires proof from both transports. If the lower is
	// intentionally stopped, keep the old address until it can be verified.
	if current.Endpoint != machine.OPCEndpoint || cacheErr != nil {
		if snapshot.State != "running" {
			return fmt.Errorf("下级未运行，暂不能核对 OPC 地址身份；保留原绑定")
		}
		proposed := current
		proposed.Endpoint = machine.OPCEndpoint
		_, password, e := r.repo.SourceCredential(readCtx, sourceID)
		if e != nil {
			return e
		}
		if e = opc.VerifyGatewayIdentity(readCtx, opc.LowerConfig{Source: proposed, Password: password, CertificateFile: r.options.CertificateFile, PrivateKeyFile: r.options.PrivateKeyFile}, machine.GatewayInstanceID); e != nil {
			return fmt.Errorf("OPC 地址身份核对失败，未应用：%w", e)
		}
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.closing.Load() {
		return domain.ErrShuttingDown
	}
	latest, ok := sourceByID(r.routes.Sources(), sourceID)
	if !ok || latest.Revision != current.Revision {
		return domain.ErrRevisionConflict
	}
	if err = r.checkSourceOperationLocked(sourceID); err != nil {
		return err
	}
	if r.routes.SourcePendingCount(sourceID) != 0 {
		return domain.ErrCommandPending
	}
	proposed := current
	proposed.Name, proposed.Endpoint = machine.Name, machine.OPCEndpoint
	sources := replaceSource(r.routes.Sources(), proposed)
	err = r.routes.CommitConfiguration(r.routes.Groups(), sources, func() error {
		updated, e := r.repo.ApplyFleetMachine(ctx, platform, current, binding, machine)
		if e == nil {
			proposed = updated
			replaceSourceInPlace(sources, updated)
		}
		return e
	})
	if err != nil {
		return err
	}
	if current.Endpoint != machine.OPCEndpoint || cacheErr != nil {
		r.stopLower(sourceID)
		if proposed.Enabled {
			if e := r.startLower(proposed); e != nil {
				r.recordLowerStartFailure(proposed, e)
			}
		}
	}
	r.audit(ctx, "fleet.binding", sourceID, proposed.Revision, map[string]any{"device_id": machine.ID, "machine_revision": machine.Revision})
	r.broker.Publish(api.Event{Type: "source.process", SourceID: sourceID, Data: map[string]any{"action": "fleet_binding"}})
	return nil
}
