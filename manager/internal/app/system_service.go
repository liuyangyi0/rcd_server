package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
	"rcd_manager/internal/opc"
	platformstorage "rcd_manager/internal/platform/storage"
)

func (r *Runtime) SystemInfo(ctx context.Context) (api.SystemInfo, error) {
	certificate, err := readCertificateInfo(r.options.CertificateFile, time.Now().UTC())
	if err != nil {
		// Keep diagnostics available when the certificate itself is the fault.
		// The read-only certificate panel reports the error without preventing
		// operators from downloading a diagnostic bundle.
		certificate = api.CertificateInfo{Error: err.Error()}
	}
	var memory goruntime.MemStats
	goruntime.ReadMemStats(&memory)
	subscribers, dropped := r.broker.Stats()
	dirtyCommands := r.tracking.audit.dirtyCount()
	dirtyAudits, _ := r.managementAudit.state()
	dirtyCatalogs, _ := r.catalogPersistence.status()
	dirtyStatusEvents, dirtyStatusAlarms, _ := r.statusHistory.state()
	r.mu.RLock()
	metrics := api.RuntimeMetrics{Goroutines: goruntime.NumGoroutine(), AllocatedBytes: memory.Alloc, HeapObjects: memory.HeapObjects,
		RefreshQueueDepth: r.refreshes.depth(), DirtyCatalogs: dirtyCatalogs, DirtyCommands: dirtyCommands,
		DirtyAudits: dirtyAudits, DirtyStatusEvents: dirtyStatusEvents, DirtyStatusAlarms: dirtyStatusAlarms,
		EventSubscribers: subscribers, DroppedEventNotifications: dropped}
	r.mu.RUnlock()
	dataBytes, _ := directorySize(r.options.DataDir)
	logBytes, _ := directorySize(r.options.LogDir)
	databaseBytes := fileSize(r.options.DatabasePath) + fileSize(r.options.DatabasePath+"-wal") + fileSize(r.options.DatabasePath+"-shm")
	diskPath := r.options.DataDir
	if strings.TrimSpace(diskPath) == "" {
		diskPath = "."
	}
	diskFree, diskErr := platformstorage.DiskFree(diskPath)
	if diskErr != nil {
		diskFree = -1
	}
	return api.SystemInfo{
		OPCEndpoint: r.options.OPCEndpoint, HTTPListen: r.options.HTTPListen, DataDirectory: r.options.DataDir,
		DatabasePath: r.options.DatabasePath, LogDirectory: r.options.LogDir,
		CommandTimeoutMS: r.options.CommandTimeout.Milliseconds(), PendingTimeoutMS: r.options.CommandPendingTTL.Milliseconds(),
		SamplingIntervalMS: r.options.SamplingInterval.Milliseconds(), PublishingIntervalMS: r.options.PublishingInterval.Milliseconds(),
		MaxSources: r.options.MaxSources, MaxSessions: r.options.MaxSessions, Certificate: certificate,
		Storage: api.StorageInfo{DataDirectoryBytes: dataBytes, DatabaseBytes: databaseBytes, LogBytes: logBytes,
			DiskFreeBytes: diskFree, AuditRetentionDays: int(r.options.AuditRetention / (24 * time.Hour)), MaxAuditRows: r.options.MaxAuditRows},
		Metrics: metrics,
	}, nil
}

func (r *Runtime) ExportConfiguration(_ context.Context) (api.ConfigurationDocument, error) {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	groups := r.routes.Groups()
	sources := r.routes.Sources()
	document := api.ConfigurationDocument{SchemaVersion: 2, ExportedAt: time.Now().UTC(),
		Groups: make([]api.GroupMutation, 0, len(groups)), Sources: make([]api.ExportSource, 0, len(sources))}
	for _, group := range groups {
		document.Groups = append(document.Groups, api.GroupMutation{ID: group.ID, Name: group.Name, DefaultSourceID: group.DefaultSourceID})
		if policy, ok := r.redundancy.configuredPolicy(group.ID); ok {
			document.Redundancy = append(document.Redundancy, policy.Clone())
		}
	}
	for _, source := range sources {
		document.Sources = append(document.Sources, api.ExportSource{ID: source.ID, GroupID: source.GroupID, Name: source.Name,
			GroupBindings: append([]domain.GroupBinding(nil), source.GroupBindings...),
			Endpoint:      source.Endpoint, SecurityPolicy: source.SecurityPolicy, SecurityMode: source.SecurityMode,
			Username: source.Username, Enabled: source.Enabled})
	}
	return document, nil
}

func (r *Runtime) ImportConfiguration(ctx context.Context, request api.ConfigurationImportRequest) (api.ConfigurationImportResult, error) {
	document := request.Document
	groups, sources, warnings, err := r.validateConfigurationDocument(document)
	if err != nil {
		return api.ConfigurationImportResult{}, err
	}
	policies, err := validateRedundancyImport(document.Redundancy, groups, sources)
	if err != nil {
		return api.ConfigurationImportResult{}, err
	}
	if len(policies) > 0 {
		warnings = append(warnings, "自动切源策略已恢复参数但保持关闭；确认物理映射后请显式开启，差异检测按原配置恢复")
	}
	existingGroups, existingSources := r.routes.Groups(), r.routes.Sources()
	result := api.ConfigurationImportResult{DryRun: request.DryRun, CreatedGroups: len(groups), CreatedSources: len(sources), Warnings: warnings}
	if len(existingGroups) > 0 || len(existingSources) > 0 {
		result.CreatedGroups, result.CreatedSources = 0, 0
		result.UpdatedGroups, result.UpdatedSources = len(groups), len(sources)
		result.Warnings = append(result.Warnings, "当前管理器已有配置：可进行预检，但为保护在线路由，导入只允许在空配置中执行")
		if request.DryRun {
			return result, nil
		}
		return api.ConfigurationImportResult{}, fmt.Errorf("configuration import requires an empty manager: %w", domain.ErrSourceInUse)
	}
	if request.DryRun {
		return result, nil
	}

	// Validate the exact RouteTable shape before committing SQLite.
	if _, err := domain.NewRouteTable(groups, sources); err != nil {
		return api.ConfigurationImportResult{}, err
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if len(r.routes.Groups()) > 0 || len(r.routes.Sources()) > 0 {
		return api.ConfigurationImportResult{}, fmt.Errorf("configuration changed during import: %w", domain.ErrSourceInUse)
	}
	persistedGroups, persistedSources, err := r.repo.ImportEmptyConfiguration(ctx, groups, sources, policies...)
	if err != nil {
		return api.ConfigurationImportResult{}, err
	}
	if err := r.routes.ApplyConfiguration(persistedGroups, persistedSources); err != nil {
		return api.ConfigurationImportResult{}, err
	}
	for _, policy := range policies {
		r.redundancy.replacePolicy(policy)
	}
	catalogs := make(map[string]domain.SourceCatalog, len(persistedSources))
	for _, source := range persistedSources {
		catalogs[source.ID] = domain.SourceCatalog{SourceID: source.ID, GroupID: source.GroupID, State: domain.CatalogDiscovering}
	}
	index, _, conflicts, err := r.buildCatalogWithCalculations(catalogs, persistedGroups)
	if err != nil {
		return api.ConfigurationImportResult{}, err
	}
	now := time.Now().UTC()
	r.mu.Lock()
	r.catalogs, r.index, r.conflictCount = catalogs, index, conflicts
	for _, source := range persistedSources {
		r.statuses[source.ID] = lowerDisconnectedStatus(source, now)
	}
	r.mu.Unlock()
	if r.running.Load() {
		for _, source := range persistedSources {
			if source.Enabled {
				if startErr := r.startLower(source); startErr != nil {
					r.recordLowerStartFailure(source, startErr)
				}
			}
		}
	}
	r.reconcileAfterConfiguration()
	r.audit(ctx, "configuration.import", "manager", 1, map[string]any{"groups": len(groups), "sources": len(sources)})
	r.broker.Publish(api.Event{Type: "configuration.changed", Data: result})
	return result, nil
}

func lowerDisconnectedStatus(source domain.Source, now time.Time) opc.LowerStatus {
	return opc.LowerStatus{SourceID: source.ID, Connection: opc.ConnectionDisconnected, Session: opc.SessionInactive,
		Subscription: opc.SubscriptionInactive, Catalog: source.CatalogState, UpdatedAt: now}
}

func (r *Runtime) validateConfigurationDocument(document api.ConfigurationDocument) ([]domain.Group, []domain.Source, []string, error) {
	if document.SchemaVersion != 1 && document.SchemaVersion != 2 {
		return nil, nil, nil, &domain.FieldError{Field: "schema_version", Message: "must be 1 or 2"}
	}
	if len(document.Groups) == 0 {
		return nil, nil, nil, &domain.FieldError{Field: "groups", Message: "must contain at least one group"}
	}
	if len(document.Sources) > r.options.MaxSources {
		return nil, nil, nil, &domain.FieldError{Field: "sources", Message: fmt.Sprintf("maximum %d sources", r.options.MaxSources)}
	}
	groups := make([]domain.Group, 0, len(document.Groups))
	groupIDs := make(map[string]struct{}, len(document.Groups))
	defaults := make(map[string]string, len(document.Groups))
	for _, mutation := range document.Groups {
		if err := validateStableID("groups.id", mutation.ID); err != nil {
			return nil, nil, nil, err
		}
		if strings.TrimSpace(mutation.Name) == "" {
			return nil, nil, nil, &domain.FieldError{Field: "groups.name", Message: "must not be empty"}
		}
		if _, duplicate := groupIDs[mutation.ID]; duplicate {
			return nil, nil, nil, &domain.FieldError{Field: "groups.id", Message: "duplicate " + mutation.ID}
		}
		groupIDs[mutation.ID] = struct{}{}
		defaults[mutation.ID] = strings.TrimSpace(mutation.DefaultSourceID)
		groups = append(groups, domain.Group{ID: mutation.ID, Name: mutation.Name, DefaultSourceID: strings.TrimSpace(mutation.DefaultSourceID), Revision: 1})
	}
	sources := make([]domain.Source, 0, len(document.Sources))
	sourceGroups := make(map[string]string, len(document.Sources))
	warnings := make([]string, 0)
	for _, exported := range document.Sources {
		mutation := api.SourceMutation{ID: exported.ID, GroupID: exported.GroupID, Name: exported.Name, Endpoint: exported.Endpoint,
			GroupBindings:  append([]domain.GroupBinding(nil), exported.GroupBindings...),
			SecurityPolicy: exported.SecurityPolicy, SecurityMode: exported.SecurityMode, Username: exported.Username, Enabled: exported.Enabled}
		if err := validateSourceMutation(mutation); err != nil {
			return nil, nil, nil, err
		}
		if _, duplicate := sourceGroups[exported.ID]; duplicate {
			return nil, nil, nil, &domain.FieldError{Field: "sources.id", Message: "duplicate " + exported.ID}
		}
		if _, exists := groupIDs[exported.GroupID]; !exists {
			return nil, nil, nil, &domain.FieldError{Field: "sources.group_id", Message: exported.GroupID + " does not exist"}
		}
		sourceGroups[exported.ID] = exported.GroupID
		for _, binding := range exported.GroupBindings {
			if _, exists := groupIDs[binding.GroupID]; !exists {
				return nil, nil, nil, &domain.FieldError{Field: "sources.group_bindings", Message: "unknown group " + binding.GroupID}
			}
		}
		sources = append(sources, domain.Source{ID: exported.ID, GroupID: exported.GroupID, Name: exported.Name,
			GroupBindings: append([]domain.GroupBinding(nil), exported.GroupBindings...),
			Endpoint:      exported.Endpoint, SecurityPolicy: exported.SecurityPolicy, SecurityMode: exported.SecurityMode,
			Username: exported.Username, Enabled: exported.Enabled, CatalogState: domain.CatalogDiscovering, Revision: 1})
		if exported.Username != "" {
			warnings = append(warnings, fmt.Sprintf("来源 %s 的密码不会包含在导出文件中，导入后需要重新填写", exported.ID))
		}
	}
	for groupID, sourceID := range defaults {
		if sourceID == "" {
			continue
		}
		defaultSource, _ := sourceByID(sources, sourceID)
		if !defaultSource.HasGroup(groupID) {
			return nil, nil, nil, &domain.FieldError{Field: "groups.default_source_id", Message: sourceID + " is not a member of " + groupID}
		}
		// Imported sources have no cached catalog and are Discovering. Keeping
		// this default would make the group API claim a route that new Sessions
		// correctly reject as unselectable. Restore topology first; the operator
		// can set the default after the first successful discovery.
		for index := range groups {
			if groups[index].ID == groupID {
				groups[index].DefaultSourceID = ""
				break
			}
		}
		warnings = append(warnings, fmt.Sprintf("分组 %s 的默认来源 %s 已暂缓；首次目录发现 Ready 后请确认并设置默认来源", groupID, sourceID))
	}
	return groups, sources, warnings, nil
}

func (r *Runtime) DiagnosticBundle(ctx context.Context) ([]byte, error) {
	system, err := r.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}
	overview, err := r.Overview(ctx)
	if err != nil {
		return nil, err
	}
	configuration, _ := r.ExportConfiguration(ctx)
	alarms, _ := r.ListAlarms(ctx, api.PageRequest{Limit: 200})
	events, _ := r.ListSourceEvents(ctx, api.PageRequest{Limit: 500})
	audit, _ := r.ListManagementAudit(ctx, api.PageRequest{Limit: 500})
	sources, _ := r.ListSources(ctx)
	groups, _ := r.ListGroups(ctx)

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entries := map[string]any{"system.json": system, "overview.json": overview, "configuration.json": configuration,
		"alarms.json": alarms, "source-events.json": events, "management-audit.json": audit,
		"sources.json": sources, "groups.json": groups, "health.json": r.Health(ctx)}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writer, createErr := archive.Create(name)
		if createErr != nil {
			_ = archive.Close()
			return nil, createErr
		}
		encoded, _ := json.MarshalIndent(entries[name], "", "  ")
		_, _ = writer.Write(encoded)
	}
	if tail, readErr := readTail(filepath.Join(r.options.LogDir, "rcd-manager.log"), 2<<20); readErr == nil && len(tail) > 0 {
		writer, _ := archive.Create("rcd-manager.log.tail")
		_, _ = writer.Write(tail)
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (r *Runtime) PrometheusMetrics(ctx context.Context) (string, error) {
	overview, err := r.Overview(ctx)
	if err != nil {
		return "", err
	}
	system, err := r.SystemInfo(ctx)
	if err != nil {
		return "", err
	}
	sources, _ := r.ListSources(ctx)
	var output strings.Builder
	writeMetric := func(name, help string, value any) {
		fmt.Fprintf(&output, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, value)
	}
	writeMetric("rcd_manager_sources_connected", "Connected downstream OPC sources.", overview.ConnectedSources)
	writeMetric("rcd_manager_alarms_open", "Open unsilenced operational alarms.", overview.OpenAlarms)
	writeMetric("rcd_manager_points_bad", "Bad-quality points on default routes.", overview.BadPoints)
	writeMetric("rcd_manager_sessions", "Active upstream OPC sessions.", overview.SessionCount)
	writeMetric("rcd_manager_event_notifications_dropped_total", "Coalesced SSE invalidations due to a full subscriber queue.", system.Metrics.DroppedEventNotifications)
	writeMetric("rcd_manager_durable_writes_pending", "Operational records waiting for durable persistence.", system.Metrics.DirtyCatalogs+system.Metrics.DirtyCommands+system.Metrics.DirtyAudits+system.Metrics.DirtyStatusEvents+system.Metrics.DirtyStatusAlarms)
	writeMetric("rcd_manager_go_goroutines", "Current Go goroutine count.", system.Metrics.Goroutines)
	writeMetric("rcd_manager_go_alloc_bytes", "Current Go allocated heap bytes.", system.Metrics.AllocatedBytes)
	for _, source := range sources {
		connected := 0
		if strings.EqualFold(source.ConnectionState, "Connected") {
			connected = 1
		}
		label := prometheusLabel(source.ID)
		fmt.Fprintf(&output, "rcd_manager_source_connected{source_id=\"%s\"} %d\n", label, connected)
		fmt.Fprintf(&output, "rcd_manager_source_bad_points{source_id=\"%s\"} %d\n", label, source.BadPoints)
		fmt.Fprintf(&output, "rcd_manager_source_reconnects_total{source_id=\"%s\"} %d\n", label, source.ReconnectCount)
	}
	return output.String(), nil
}

func readCertificateInfo(path string, now time.Time) (api.CertificateInfo, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return api.CertificateInfo{}, fmt.Errorf("read OPC certificate: %w", err)
	}
	block, _ := pem.Decode(content)
	if block == nil {
		return api.CertificateInfo{}, fmt.Errorf("decode OPC certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return api.CertificateInfo{}, err
	}
	fingerprint := sha256.Sum256(certificate.Raw)
	result := api.CertificateInfo{Subject: certificate.Subject.String(), SerialNumber: certificate.SerialNumber.String(),
		SHA256: strings.ToUpper(hex.EncodeToString(fingerprint[:])), NotBefore: certificate.NotBefore.UTC(), NotAfter: certificate.NotAfter.UTC(),
		DaysRemaining: int(certificate.NotAfter.Sub(now).Hours() / 24),
		ValidNow:      !now.Before(certificate.NotBefore) && now.Before(certificate.NotAfter)}
	result.ExpiryWarning = result.DaysRemaining <= 90
	for _, uri := range certificate.URIs {
		result.ApplicationURIs = append(result.ApplicationURIs, uri.String())
	}
	return result, nil
}

func directorySize(root string) (int64, error) {
	if strings.TrimSpace(root) == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			if info, infoErr := entry.Info(); infoErr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func fileSize(path string) int64 {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func readTail(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, 0); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func prometheusLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
