import { describe, expect, it } from 'vitest';
import { normalizeAlarm, normalizeCommand, normalizeOverview, normalizePage, normalizePoint, normalizeSession, normalizeSource, normalizeSourceProbe, normalizeSourceTopology, normalizeSystemInfo } from './normalizers';

describe('API normalizers', () => {
  it('maps device bindings and distinguishes configured from discovered group membership', () => {
    const source = normalizeSource({ id: 'lower-1', group_id: 'fallback', group_bindings: [{ group_id: 'device-a', port_id: '/dev/ttyS0', device_id: '1' }], available_group_ids: ['fallback', 'device-a'] });
    expect(source.groupId).toBe('fallback');
    expect(source.groupBindings).toEqual([{ groupId: 'device-a', portId: '/dev/ttyS0', deviceId: '1' }]);
    expect(source.availableGroupIds).toEqual(['fallback', 'device-a']);
    expect(normalizeSource({ group_id: 'legacy' }).groupBindings).toEqual([]);
    expect(normalizeSource({ available_group_ids: [] }).availableGroupIds).toEqual([]);
  });

  it('normalizes topology summaries without copying full node collections', () => {
    const topology = normalizeSourceTopology({ source_id: 'lower-1', group_id: 'fallback', revision: 8, schema_version: 1, supported: true, catalog_state: 'Cached', calculation_count: 3, cross_device_calculation_count: 1, unassigned_point_count: 2, ports: [{ port_id: '/dev/ttyS0', port_name: 'ttyS0', point_count: 5300, devices: [{ device_id: '1', device_address: 1, name: '设备 A', group_id: 'device-a', bound: true, point_count: 5300, calculation_count: 1, node_ids: ['ignored'] }] }] });
    expect(topology).toMatchObject({ sourceId: 'lower-1', groupId: 'fallback', revision: 8, supported: true, catalogState: 'Cached', crossDeviceCalculationCount: 1 });
    expect(topology.ports[0].devices[0]).toMatchObject({ deviceId: '1', deviceAddress: 1, groupId: 'device-a', bound: true, pointCount: 5300 });
    expect(topology.ports[0].devices[0]).not.toHaveProperty('node_ids');
  });

  it('reads source-specific point ownership from the returned catalog node', () => {
    const point = normalizePoint({ group_id: 'device-a', selected_source_id: 'lower-b', source_present: true, node: { key: { identifier: 'TEMP' }, group_id: 'device-a', port_id: '/dev/ttyUSB4', device_id: '2', kind: 'calculation' } });
    expect(point).toMatchObject({ groupId: 'device-a', portId: '/dev/ttyUSB4', deviceId: '2', kind: 'calculation' });
  });

  it('maps the concrete Go session route shape', () => {
    const session = normalizeSession({
      id: 's-1', activated_at: '2026-08-24T01:00:00Z', last_seen_at: '2026-08-24T01:01:00Z',
      routes: { fire: { group_id: 'fire', source_id: 'rcd-a-02', revision: 7 } },
      client_application_uri: 'urn:test:scada', remote_address: '10.0.0.5:50000',
    });
    expect(session.clientName).toBe('urn:test:scada');
    expect(session.selectedSources.fire).toBe('rcd-a-02');
    expect(session.routeRevisions.fire).toBe(7);
  });

  it('maps nested catalog node and sample values', () => {
    const point = normalizePoint({
      group_id: 'fire', selected_source_id: 'rcd-a-01',
      node: { key: { namespace_uri: 'urn:SerialServer', identifier: 'KKS-001' }, browse_name: 'KKS-001', data_type: 'Double', retired: false },
      sample: { type: 'Double', value: 12.5, status_code: 0x80310000, source_timestamp: '2026-08-24T01:00:00Z' },
    });
    expect(point.nodeId).toBe('KKS-001');
		expect(point.nodeClass).toBe('Variable');
    expect(point.value).toBe(12.5);
    expect(point.statusCode).toBe('BadNoCommunication');
  });

	it('preserves Method node class so the UI does not present it as a sampled variable', () => {
		const point = normalizePoint({
			group_id: 'fire', node: { key: { identifier: 'COM1_SendCommand' }, class: 'Method', browse_name: 'COM1_SendCommand' },
			sample: { status_code: 0x80320000 },
		});
		expect(point.nodeClass).toBe('Method');
	});

	it('derives command duration from persisted start and finish timestamps', () => {
		const command = normalizeCommand({
			id: 'cmd-1', started_at: '2026-08-25T03:00:00.000Z', finished_at: '2026-08-25T03:00:00.091Z',
			kind: 'Write', state: 'Failed', status_code: 0x800a0000,
		});
		expect(command.durationMs).toBe(91);
		expect(command.statusCode).toBe('BadTimeout');
	});

  it('converts limit and offset into a UI page', () => {
    const result = normalizePage({ items: [{ id: 1 }], total: 101, limit: 50, offset: 100 }, (item) => item);
    expect(result.page).toBe(3);
    expect(result.pageSize).toBe(50);
    expect(result.total).toBe(101);
  });

  it('maps the concrete overview counters without inventing quality', () => {
    const overview = normalizeOverview({ source_count: 6, connected_sources: 5, point_count: 10633, session_count: 2, started_at: new Date(Date.now() - 10_000).toISOString() });
    expect(overview.totalSources).toBe(6);
    expect(overview.connectedSources).toBe(5);
    expect(overview.abnormalSources).toBe(1);
    expect(overview.totalPoints).toBe(10633);
    expect(overview.goodPoints).toBe(0);
  });

  it('preserves source identity and catalog conflict diagnostics', () => {
    const source = normalizeSource({
      id: 'rcd-a-01', group_id: 'fire', username: 'operator', has_password: true,
      catalog_state: 'CatalogConflict',
      catalog_conflicts: [{
        kind: 'TypeMismatch', group_id: 'fire', identifier: 'KKS-001',
        source_ids: ['rcd-a-01', 'rcd-a-02'], message: 'data type differs',
      }],
    });
    expect(source.username).toBe('operator');
    expect(source.hasPassword).toBe(true);
    expect(source.catalogConflicts).toEqual([expect.objectContaining({
      kind: 'TypeMismatch', identifier: 'KKS-001', sourceIds: ['rcd-a-01', 'rcd-a-02'],
    })]);
  });

  it('normalizes operational alarm, source probe and system diagnostics', () => {
    const alarm = normalizeAlarm({ id: 4, source_id: 'rcd-a', kind: 'source.connection', severity: 'Critical', state: 'Open', occurrences: 3, opened_at: '2026-08-25T01:00:00Z', updated_at: '2026-08-25T01:02:00Z', revision: 2 });
    expect(alarm).toMatchObject({ id: 4, sourceId: 'rcd-a', occurrences: 3, revision: 2 });
    const probe = normalizeSourceProbe({ source_id: 'rcd-a', ok: true, endpoint_count: 2, variable_count: 31, method_count: 1, duration_ms: 125, tested_at: '2026-08-25T01:00:00Z', stages: [{ name: 'DiscoverEndpoints', ok: true, duration_ms: 20 }] });
    expect(probe).toMatchObject({ sourceId: 'rcd-a', ok: true, endpointCount: 2, variableCount: 31 });
    const system = normalizeSystemInfo({ opc_endpoint: 'opc.tcp://manager:4840', certificate: { error: 'certificate missing' }, storage: { disk_free_bytes: 1024, max_audit_rows: 100000 }, metrics: { goroutines: 20, dropped_event_notifications: 2 } });
    expect(system.certificate.error).toBe('certificate missing');
    expect(system.storage.diskFreeBytes).toBe(1024);
    expect(system.metrics.droppedEventNotifications).toBe(2);
  });
});
