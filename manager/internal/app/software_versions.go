package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"rcd_manager/internal/buildinfo"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	"sort"
	"sync"
	"time"
)

type softwareAttempt struct {
	revision       uint64
	key            string
	checked        time.Time
	state, message string
	epoch          uint64
	value          *domain.SoftwareVersion
}
type softwareVersions struct {
	refresh     sync.Mutex
	mu          sync.Mutex
	lastRefresh time.Time
	attempts    map[string]softwareAttempt
}

func softwareKey(source domain.Source, component string, binding domain.ProcessBinding) string {
	parts := []any{source.CreatedAt, source.Endpoint, source.SecurityMode, source.SecurityPolicy, source.Username}
	if component == "agent" {
		parts = []any{source.CreatedAt, binding.AgentURL, binding.InstanceID, binding.Revision}
	}
	raw, _ := json.Marshal(parts)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
func safeSoftwareEndpoint(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (r *Runtime) softwareVersionWorker() {
	defer r.wg.Done()
	r.mu.RLock()
	ctx := r.runContext
	r.mu.RUnlock()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.refreshSoftwareVersions(ctx)
			timer.Reset(time.Minute)
		}
	}
}

func (r *Runtime) refreshSoftwareVersions(ctx context.Context) {
	if r.closing.Load() || !r.software.refresh.TryLock() {
		return
	}
	defer r.software.refresh.Unlock()
	// Multiple viewers do not multiply OPC/HTTP traffic. Never overlap batches.
	if time.Since(r.software.lastRefresh) < 10*time.Second {
		return
	}
	r.software.lastRefresh = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sources := r.routes.Sources()
	active := make(map[string]bool, len(sources)*2)
	for _, source := range sources {
		active[source.ID+"/lower"] = true
		active[source.ID+"/agent"] = true
	}
	r.software.mu.Lock()
	for key := range r.software.attempts {
		if !active[key] {
			delete(r.software.attempts, key)
		}
	}
	r.software.mu.Unlock()
	jobs := make(chan domain.Source)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for source := range jobs {
				if ctx.Err() != nil {
					return
				}
				r.refreshSoftwareSource(ctx, source, "lower")
				r.refreshSoftwareSource(ctx, source, "agent")
			}
		}()
	}
	for _, source := range sources {
		select {
		case jobs <- source:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (r *Runtime) refreshSoftwareSource(ctx context.Context, source domain.Source, component string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	binding, err := r.repo.GetProcessBinding(ctx, source.ID)
	if err != nil {
		return
	}
	key := softwareKey(source, component, binding)
	attempt := softwareAttempt{key: key, revision: source.Revision, checked: time.Now().UTC(), state: "offline", message: "读取失败；无法确认当前运行版本"}
	value := domain.SoftwareVersion{SourceID: source.ID, Component: component}
	if component == "agent" {
		if !binding.Configured() {
			attempt.state = "unconfigured"
			attempt.message = "尚未配置 Agent 绑定"
		} else {
			snapshot, readErr := r.options.ProcessAgentClient.Status(ctx, binding)
			if readErr == nil {
				readErr = snapshot.Validate(binding.InstanceID)
			}
			if readErr == nil {
				if identity := snapshot.AgentIdentity; identity != nil {
					info := identity.Build
					value.Build = &info
					value.BootID = identity.BootID
					start := identity.StartedAt
					value.StartedAt = &start
				} else if client, ok := r.options.ProcessAgentClient.(interface {
					SoftwareVersion(context.Context, domain.ProcessBinding) (buildinfo.Info, error)
				}); ok {
					info, versionErr := client.SoftwareVersion(ctx, binding)
					if versionErr == nil {
						value.Build = &info
					}
				}
				if value.Build == nil {
					attempt.state = "unsupported"
					attempt.message = "Agent 未提供可验证的软件版本"
				}
			}
		}
	} else if !source.Enabled {
		attempt.state = "disabled"
		attempt.message = "来源已停用"
	} else {
		r.configMu.RLock()
		target, targetErr := r.lowerOperationsTargetLocked(source.ID)
		r.configMu.RUnlock()
		var operationError *domain.OperationError
		if errors.As(targetErr, &operationError) && operationError.Code == "unsupported" {
			attempt.state = "unsupported"
			attempt.message = "下级不支持软件版本查询"
		}
		if targetErr == nil {
			attempt.epoch = target.connectionEpoch
			capabilities, readErr := target.client.OperationsCapabilities(ctx, target.epoch)
			if readErr == nil && (!capabilities.Supported || (len(capabilities.Features) != 0 && !capabilities.HasFeature("acquisition"))) {
				readErr = &domain.OperationError{Code: "unsupported", Message: "该来源未提供运行版本状态接口"}
			}
			var raw json.RawMessage
			if readErr == nil {
				// Status belongs to the existing acquisition feature. Older v1
				// peers with an empty feature list retain their status probe;
				// configuration-only peers must not receive an unsupported Call.
				raw, readErr = target.client.ExecuteOperation(ctx, domain.OperationRequest{Action: "status"}, target.epoch)
			}
			var operationError *domain.OperationError
			if errors.As(readErr, &operationError) && operationError.Code == "unsupported" {
				attempt.state = "unsupported"
				attempt.message = "该来源未提供可验证的运行版本；业务 OPC 连接状态请查看来源页面"
			}
			if readErr == nil {
				var status struct {
					Build   *buildinfo.Info `json:"build_info"`
					Version string          `json:"software_version"`
					Commit  string          `json:"build_commit"`
				}
				if json.Unmarshal(raw, &status) == nil {
					value.Build = status.Build
					if value.Build == nil && status.Version != "" {
						value.Build = &buildinfo.Info{SchemaVersion: 1, Component: "lower", Version: status.Version, Commit: status.Commit}
					}
				}
				if value.Build == nil {
					attempt.state = "unsupported"
					attempt.message = "下级未提供软件版本"
				}
			}
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if value.Build != nil && domain.ValidateSoftwareBuild(*value.Build, component) != nil {
		value.Build = nil
		attempt.state = "invalid"
		attempt.message = "版本响应格式或组件身份不匹配"
	}
	if value.Build != nil {
		if component == "lower" {
			r.mu.RLock()
			status := r.statuses[source.ID]
			r.mu.RUnlock()
			if status.Epoch != attempt.epoch || status.Connection != opc.ConnectionConnected {
				return
			}
		}
		value.ObservedAt = time.Now().UTC()
		if err := r.repo.SaveSoftwareVersion(ctx, source, binding, key, value); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return
			}
			attempt.message = "版本已读取，但缓存保存失败；管理端重启后可能丢失本次记录"
		} else {
			attempt.message = ""
		}
		attempt.state = "online"
		attempt.value = &value
	}
	r.software.mu.Lock()
	defer r.software.mu.Unlock()
	if r.software.attempts == nil {
		r.software.attempts = make(map[string]softwareAttempt)
	}
	r.software.attempts[source.ID+"/"+component] = attempt
}

func (r *Runtime) SoftwareVersions(ctx context.Context, refresh bool) ([]domain.SoftwareVersion, error) {
	if refresh {
		r.refreshSoftwareVersions(ctx)
	}
	info := buildinfo.Current()
	started := r.startedAt
	values := []domain.SoftwareVersion{{Name: "当前管理端", Component: "manager", Endpoint: safeSoftwareEndpoint(r.options.OPCEndpoint), Build: &info, StartedAt: &started, ObservedAt: time.Now().UTC(), State: "online"}}
	sources := r.routes.Sources()
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	for _, source := range sources {
		binding, err := r.repo.GetProcessBinding(ctx, source.ID)
		if errors.Is(err, domain.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, component := range []string{"lower", "agent"} {
			key := softwareKey(source, component, binding)
			value, err := r.repo.LoadSoftwareVersion(ctx, source.ID, component, key)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			r.software.mu.Lock()
			attempt := r.software.attempts[source.ID+"/"+component]
			r.software.mu.Unlock()
			value.State = "unknown"
			value.Cached = value.Build != nil
			if value.Cached {
				value.State = "cached"
				value.Message = "上次确认的版本，等待重新读取"
			}
			if attempt.key == key && attempt.revision == source.Revision {
				value.CheckedAt = attempt.checked
				value.State = attempt.state
				value.Message = attempt.message
				if attempt.value != nil {
					value = *attempt.value
					value.CheckedAt = attempt.checked
					value.State = attempt.state
					value.Message = attempt.message
					value.Cached = false
				}
				if time.Since(attempt.checked) > 90*time.Second {
					value.Cached = value.Build != nil
					value.State = "cached"
					value.Message = "版本确认已过期"
				}
			}
			if component == "lower" {
				r.mu.RLock()
				status := r.statuses[source.ID]
				r.mu.RUnlock()
				if !source.Enabled {
					value.State = "disabled"
					value.Cached = value.Build != nil
				} else if status.Connection != opc.ConnectionConnected {
					value.State = "offline"
					value.Cached = value.Build != nil
				} else if attempt.key == key && attempt.epoch != status.Epoch {
					value.State = "cached"
					value.Cached = value.Build != nil
					value.Message = "连接已变化，等待重新确认版本"
				}
				value.Endpoint = safeSoftwareEndpoint(source.Endpoint)
			} else {
				value.Endpoint = safeSoftwareEndpoint(binding.AgentURL)
				if !binding.Configured() {
					value = domain.SoftwareVersion{State: "unconfigured", Message: "尚未配置 Agent 绑定"}
				}
			}
			value.SourceID = source.ID
			value.Name = source.Name
			value.Component = component
			// Return an owned copy, never expose the service's immutable identity pointer.
			if value.Build != nil {
				copy := *value.Build
				value.Build = &copy
			}
			if value.StartedAt != nil {
				start := *value.StartedAt
				value.StartedAt = &start
			}
			values = append(values, value)
		}
	}
	return values, nil
}
