import { useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { CheckCircle2, Download, RefreshCw, Search, Settings2, XCircle } from 'lucide-react';
import { useSearchParams } from 'react-router-dom';
import { operationsApi, useDeviceVersionMutation, type AcquisitionSnapshot, type DeviceVersionMutationInput } from '../api/operations';
import { queryKeys, useDeviceVersions, useSources } from '../api/queries';
import type { DeviceVersion, DeviceVersionParams, DeviceVersionSummary, ModuleType } from '../api/types';
import { InlineNotice } from '../components/InlineNotice';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { Pagination } from '../components/Pagination';
import { StatusBadge } from '../components/StatusBadge';
import { ApiError, apiErrorMessage } from '../api/client';
import { formatDateTime } from '../lib/format';

const PAGE_SIZE = 50;
const SOURCE_CONCURRENCY = 3;
const MAX_REVISION_ATTEMPTS = 3;

type RowOperation = { state: 'running' | 'success' | 'warning' | 'error'; message: string };

export default function DeviceVersionsPage() {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const [draftQuery, setDraftQuery] = useState(searchParams.get('q') ?? '');
  const sourceId = searchParams.get('source') ?? '';
  const portId = searchParams.get('port') ?? '';
  const moduleType = parseModuleType(searchParams.get('type'));
  const status = searchParams.get('status') ?? '';
  const queryText = searchParams.get('q') ?? '';
  const page = Math.max(1, Number(searchParams.get('page') ?? 1) || 1);
  const filterKey = searchParams.toString();
  const params = useMemo<DeviceVersionParams>(() => ({
    page, pageSize: PAGE_SIZE, query: queryText, sourceId, portId, moduleType, status,
  }), [moduleType, page, portId, queryText, sourceId, status]);
  const versionsQuery = useDeviceVersions(params);
  const sourcesQuery = useSources();
  const mutation = useDeviceVersionMutation();
  const [selected, setSelected] = useState<Map<string, DeviceVersion>>(() => new Map());
  const [typeTargets, setTypeTargets] = useState<DeviceVersion[]>([]);
  const [nextType, setNextType] = useState<ModuleType>('domestic');
  const [typeConfirmed, setTypeConfirmed] = useState(false);
  const [queryTargets, setQueryTargets] = useState<DeviceVersion[]>([]);
  const [queryConfirmed, setQueryConfirmed] = useState(false);
  const [rowOperations, setRowOperations] = useState<Map<string, RowOperation>>(() => new Map());
  const [batchRunning, setBatchRunning] = useState(false);
  const rows = versionsQuery.data?.items ?? [];
  const selectableRows = useMemo(() => rows.filter((row) => row.versionSupported && Boolean(row.stationRevision)), [rows]);
  const pageKeys = useMemo(() => new Set(rows.map(versionKey)), [rows]);
  const selectedOnPage = useMemo(() => [...selected.values()].filter((row) => pageKeys.has(versionKey(row))), [pageKeys, selected]);
  const selectableDomestic = useMemo(() => [...selected.values()].filter(canQueryVersion), [selected]);
  const allPageSelected = selectableRows.length > 0 && selectableRows.every((row) => selected.has(versionKey(row)));
  const ports = useMemo(() => [...new Map(rows.map((row) => [row.portId, row.portName || row.portId])).entries()].sort((a, b) => a[1].localeCompare(b[1])), [rows]);

  useEffect(() => setDraftQuery(queryText), [filterKey, queryText]);
  useEffect(() => { setSelected(new Map()); }, [filterKey]);

  const updateParams = (next: Record<string, string | number | undefined>) => {
    const updated = new URLSearchParams(searchParams);
    for (const [key, value] of Object.entries(next)) {
      if (value === undefined || value === '' || (key === 'page' && value === 1)) updated.delete(key);
      else updated.set(key, String(value));
    }
    setSearchParams(updated);
  };

  const updateRowOperation = (row: DeviceVersion, operation: RowOperation) => {
    setRowOperations((previous) => new Map(previous).set(versionKey(row), operation));
  };

  const execute = async (targets: DeviceVersion[], action: 'set_type' | 'query', configuredType?: ModuleType) => {
    if (targets.length === 0) return;
    setBatchRunning(true);
    const bySource = new Map<string, DeviceVersion[]>();
    for (const target of uniqueVersions(targets)) {
      const sourceRows = bySource.get(target.sourceId) ?? [];
      sourceRows.push(target);
      bySource.set(target.sourceId, sourceRows);
    }
    const jobs = [...bySource.values()].map((sourceRows) => async () => {
      let stationRevision = sourceRows[0].stationRevision;
      for (let index = 0; index < sourceRows.length; index += 1) {
        const row = sourceRows[index];
        updateRowOperation(row, { state: 'running', message: action === 'query' ? '正在读取版本…' : '正在保存类型…' });
        let input: DeviceVersionMutationInput = action === 'query'
          ? { action: 'query', target: { id: row.sourceId, revision: row.sourceRevision }, stationRevision, portId: row.portId, deviceId: row.deviceId, deferInvalidation: true }
          : { action: 'set_type', target: { id: row.sourceId, revision: row.sourceRevision }, stationRevision, portId: row.portId, deviceId: row.deviceId, moduleType: configuredType ?? 'unknown', deferInvalidation: true };
        try {
          let result: AcquisitionSnapshot | undefined;
          for (let attempt = 1; attempt <= MAX_REVISION_ATTEMPTS; attempt += 1) {
            try {
              result = await mutation.mutateAsync(input);
              break;
            } catch (error) {
              // Only a confirmed revision rejection is safe to repeat. An
              // automatic probe may advance this source-wide revision more
              // than once; every retry must observe a strictly newer revision.
              if (!(error instanceof ApiError) || error.status !== 409 || attempt === MAX_REVISION_ATTEMPTS) throw error;
              const latest = await operationsApi.status(row.sourceId, undefined, row.sourceRevision);
              if (!latest.station_revision || !stationRevisionAdvanced(stationRevision, latest.station_revision)) throw error;
              stationRevision = latest.station_revision;
              input = { ...input, stationRevision };
            }
          }
          if (!result) throw new Error('设备版本操作没有返回结果');
          stationRevision = result.station_revision || stationRevision;
          const station = findStationVersion(result, row);
          const versionState = station?.version_state?.toLowerCase() ?? '';
          const waiting = ['waiting', 'pending', 'querying'].some((state) => versionState.includes(state));
          const failed = ['fail', 'error', 'timeout'].some((state) => versionState.includes(state));
          if (result.status === 'Pending' || (action === 'query' && waiting)) {
            updateRowOperation(row, { state: 'warning', message: action === 'query' ? '查询已受理，等待下级完成并刷新结果' : '设置已受理，等待下级应用' });
            for (const skipped of sourceRows.slice(index + 1)) updateRowOperation(skipped, { state: 'warning', message: '同一来源前一项仍待完成，未继续提交' });
            break;
          } else if (action === 'query' && station?.type_mismatch) {
            updateRowOperation(row, { state: 'warning', message: station.last_version_error || '查询完成，但实测模块类型与配置不一致' });
          } else if (action === 'query' && (station?.last_version_error || failed)) {
            updateRowOperation(row, { state: 'warning', message: station?.last_version_error ? `查询未成功：${station.last_version_error}` : '版本查询未成功，请刷新状态查看详情' });
          } else {
            updateRowOperation(row, { state: 'success', message: action === 'query' ? '版本查询完成' : '模块类型已保存' });
          }
        } catch (error) {
          updateRowOperation(row, { state: 'error', message: apiErrorMessage(error) });
          for (const skipped of sourceRows.slice(index + 1)) updateRowOperation(skipped, { state: 'error', message: '同一来源前一项失败，为避免沿用过期版本，未继续执行' });
          break;
        }
      }
    });
    try {
      await runWithConcurrency(jobs, SOURCE_CONCURRENCY);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.allDeviceVersions }),
        queryClient.invalidateQueries({ queryKey: queryKeys.allManagementAudit, refetchType: 'active' }),
      ]);
    } finally {
      setBatchRunning(false);
      setSelected(new Map());
    }
  };

  const closeTypeDialog = () => {
    if (batchRunning) return;
    setTypeTargets([]);
    setTypeConfirmed(false);
  };
  const closeQueryDialog = () => {
    if (batchRunning) return;
    setQueryTargets([]);
    setQueryConfirmed(false);
  };

  return <div className="page device-versions-page">
    <PageHeader title="设备版本" description="按下级来源、物理串口和站点管理国产/俄供通信模块；版本信息不参与 OPC 分组选源。" actions={<button className="button button-outline" type="button" disabled={rows.length === 0} onClick={() => exportVersionCsv(rows)}><Download size={16} aria-hidden="true" />导出当前页 CSV</button>} />

    <VersionSummary summary={versionsQuery.data?.summary} loading={versionsQuery.isPending} />

    <form className="toolbar filter-toolbar device-version-toolbar" onSubmit={(event) => { event.preventDefault(); updateParams({ q: draftQuery.trim(), page: 1 }); }}>
      <label className="search-field"><Search aria-hidden="true" size={17} /><input value={draftQuery} placeholder="搜索来源、串口、站点或版本" onChange={(event) => setDraftQuery(event.target.value)} /><button type="submit">搜索</button></label>
      <label className="field inline-field"><span>下级来源</span><select value={sourceId} onChange={(event) => updateParams({ source: event.target.value, port: '', page: 1 })}><option value="">全部来源</option>{(sourcesQuery.data ?? []).map((source) => <option key={source.id} value={source.id}>{source.name || source.id}</option>)}</select></label>
      <label className="field inline-field"><span>串口</span><select value={portId} onChange={(event) => updateParams({ port: event.target.value, page: 1 })}><option value="">全部串口</option>{portId && !ports.some(([id]) => id === portId) ? <option value={portId}>{portId}</option> : null}{ports.map(([id, name]) => <option key={id} value={id}>{name}</option>)}</select></label>
      <label className="field inline-field"><span>配置类型</span><select value={moduleType} onChange={(event) => updateParams({ type: event.target.value, page: 1 })}><option value="">全部类型</option><option value="domestic">国产</option><option value="legacy_russian">俄供</option><option value="unknown">未配置</option></select></label>
      <label className="field inline-field"><span>状态</span><select value={status} onChange={(event) => updateParams({ status: event.target.value, page: 1 })}><option value="">全部状态</option><option value="success">版本有效</option><option value="mismatch">类型不一致</option><option value="failed">查询失败</option><option value="stale">数据已过期</option><option value="offline_cached">离线缓存</option></select></label>
      <button className="button button-secondary" type="button" disabled={versionsQuery.isFetching || batchRunning} onClick={() => void versionsQuery.refetch()}><RefreshCw className={versionsQuery.isFetching ? 'spin' : undefined} size={16} aria-hidden="true" />刷新</button>
    </form>

    <div className="device-version-batchbar">
      <span>已选择 <strong>{selected.size}</strong> 个站点{selected.size !== selectedOnPage.length ? `（当前页 ${selectedOnPage.length} 个）` : ''}</span>
      <div>
        <button className="button button-small button-outline" type="button" disabled={selected.size === 0 || batchRunning} onClick={() => { setTypeTargets([...selected.values()]); setNextType('domestic'); setTypeConfirmed(false); }}>批量设置类型</button>
        <button className="button button-small button-primary" type="button" disabled={selectableDomestic.length === 0 || batchRunning} title={selected.size > 0 && selectableDomestic.length === 0 ? '只有在线、启用且已配置为国产的站点可以查询' : undefined} onClick={() => { setQueryTargets(selectableDomestic); setQueryConfirmed(false); }}>查询国产版本{selectableDomestic.length ? ` (${selectableDomestic.length})` : ''}</button>
        <button className="button button-small button-secondary" type="button" disabled={selected.size === 0 || batchRunning} onClick={() => setSelected(new Map())}>清除选择</button>
      </div>
    </div>

    <section className="table-panel content-panel">
      {versionsQuery.error ? <ErrorState error={versionsQuery.error} onRetry={() => void versionsQuery.refetch()} /> : versionsQuery.isPending && !versionsQuery.data ? <LoadingState label="正在读取全部下级设备版本…" /> : rows.length === 0 ? <EmptyState title="没有匹配的设备版本" description={params.query || sourceId || portId || moduleType || status ? '请调整搜索或筛选条件。' : '新版下级连接并完成站点发现后，设备会显示在这里。'} /> : <>
        <div className={`table-scroll${versionsQuery.isFetching ? ' is-refreshing' : ''}`}>
          <table className="data-table device-version-table">
            <thead><tr><th className="selection-column"><input type="checkbox" aria-label="选择当前页全部站点" checked={allPageSelected} disabled={selectableRows.length === 0} onChange={(event) => setSelected((previous) => togglePage(previous, selectableRows, event.target.checked))} /></th><th>下级 / 程序版本</th><th>串口 / 站点</th><th>配置类型</th><th>实测类型</th><th>通信模块</th><th>PLC</th><th>版本状态</th><th>最后查询</th><th>操作</th></tr></thead>
            <tbody>{rows.map((row) => {
              const key = versionKey(row);
              const operation = rowOperations.get(key);
              const queryDisabledReason = queryDisabled(row);
              return <tr key={key} className="virtual-row">
                <td className="selection-column"><input type="checkbox" aria-label={`选择 ${row.sourceName || row.sourceId} ${row.portId} ${row.deviceName || row.deviceId}`} checked={selected.has(key)} disabled={!row.versionSupported || !row.stationRevision} title={!row.versionSupported ? '此下级不支持设备版本管理' : !row.stationRevision ? '等待下级返回站点版本状态' : undefined} onChange={(event) => setSelected((previous) => toggleRow(previous, row, event.target.checked))} /></td>
                <td><strong>{row.sourceName || row.sourceId}</strong><span className="sub-cell">{row.sourceId}</span><span className="sub-cell">RCD {row.rcdSoftwareVersion || '版本未知'}{row.rcdBuildCommit ? ` · ${row.rcdBuildCommit.slice(0, 8)}` : ''}</span>{!row.sourceOnline ? <span className="sub-cell text-warning">下级离线</span> : null}</td>
                <td><strong>{row.portName || row.portId}</strong><span className="sub-cell">{row.portId}</span><span className="sub-cell">{row.deviceName || '站点'} · {row.deviceId}</span>{row.sourceOnline && !row.communicationOK ? <span className="sub-cell text-warning">站点通信未确认</span> : null}</td>
                <td>{moduleTypeLabel(row.configuredModuleType)}</td>
                <td>{moduleTypeLabel(row.detectedModuleType)}{row.typeMismatch ? <span className="sub-cell text-bad">与配置不一致</span> : null}</td>
                <td><VersionValue value={row.communicationModuleVersion} raw={row.communicationModuleVersionRaw} /></td>
                <td><VersionValue value={row.plcVersion} raw={row.plcVersionRaw} /></td>
                <td><VersionState row={row} />{row.versionPayload ? <span className="sub-cell version-payload">{row.versionPayload}</span> : null}{row.lastVersionError ? <span className="sub-cell text-bad version-error">{row.lastVersionError}</span> : null}</td>
                <td>{formatDateTime(row.lastVersionAttemptAt)}{row.lastVersionSuccessAt && row.lastVersionSuccessAt !== row.lastVersionAttemptAt ? <span className="sub-cell">成功：{formatDateTime(row.lastVersionSuccessAt)}</span> : null}</td>
                <td><div className="device-version-actions"><button className="table-action-link" type="button" disabled={!row.versionSupported || !row.stationRevision || !row.sourceOnline || batchRunning} title={!row.versionSupported ? '此下级不支持设备版本管理' : !row.stationRevision ? '等待下级返回站点版本状态' : !row.sourceOnline ? '下级来源离线' : undefined} onClick={() => { setTypeTargets([row]); setNextType(row.configuredModuleType); setTypeConfirmed(false); }}><Settings2 size={13} aria-hidden="true" />设置类型</button><button className="table-action-link" type="button" disabled={Boolean(queryDisabledReason) || batchRunning} title={queryDisabledReason} onClick={() => { setQueryTargets([row]); setQueryConfirmed(false); }}><RefreshCw size={13} aria-hidden="true" />查询版本</button></div>{operation ? <RowOperationStatus operation={operation} /> : null}</td>
              </tr>;
            })}</tbody>
          </table>
        </div>
        <Pagination page={versionsQuery.data?.page ?? page} pageSize={versionsQuery.data?.pageSize ?? PAGE_SIZE} total={versionsQuery.data?.total ?? 0} onPageChange={(nextPage) => updateParams({ page: nextPage })} />
      </>}
    </section>

    <Modal open={typeTargets.length > 0} title={typeTargets.length === 1 ? '设置站点模块类型' : `批量设置 ${typeTargets.length} 个站点`} description="类型保存到物理站点，不影响 OPC 分组或当前选源。" onClose={closeTypeDialog} footer={<><button className="button button-secondary" type="button" disabled={batchRunning} onClick={closeTypeDialog}>取消</button><button className="button button-primary" type="button" disabled={batchRunning || (nextType === 'domestic' && !typeConfirmed)} onClick={() => void execute(typeTargets, 'set_type', nextType).finally(closeTypeDialog)}>{batchRunning ? '正在保存…' : '确认保存'}</button></>}>
      <label className="field"><span>模块类型</span><select value={nextType} disabled={batchRunning} onChange={(event) => { setNextType(event.target.value as ModuleType); setTypeConfirmed(false); }}><option value="unknown">未配置（不发送版本请求）</option><option value="legacy_russian">俄供原模块（不发送版本请求）</option><option value="domestic">国产自研模块</option></select></label>
      {nextType === 'domestic' ? <InlineNotice tone="warning">保存为国产后，下级会在安全轮询点主动查询一次版本号；同一串口仍保持严格串行。</InlineNotice> : <InlineNotice>未知和俄供类型不会发送地址 50 10 的版本查询。</InlineNotice>}
      {nextType === 'domestic' ? <label className="operations-check"><input type="checkbox" checked={typeConfirmed} onChange={(event) => setTypeConfirmed(event.target.checked)} />我确认这些站点为国产模块，并允许下级发送一次版本请求</label> : null}
      <TargetPreview rows={typeTargets} />
    </Modal>

    <Modal open={queryTargets.length > 0} title={queryTargets.length === 1 ? '查询设备版本' : `查询 ${queryTargets.length} 个国产站点`} description="请求会插入正常轮询之间，不会并发占用同一物理串口。" onClose={closeQueryDialog} footer={<><button className="button button-secondary" type="button" disabled={batchRunning} onClick={closeQueryDialog}>取消</button><button className="button button-primary" type="button" disabled={!queryConfirmed || batchRunning} onClick={() => void execute(queryTargets, 'query').finally(closeQueryDialog)}>{batchRunning ? '正在查询…' : '开始查询'}</button></>}>
      <InlineNotice tone="warning">查询使用现场串口。单站超时不会自动重试，最后成功版本会保留并标记过期。</InlineNotice>
      <label className="operations-check"><input type="checkbox" checked={queryConfirmed} onChange={(event) => setQueryConfirmed(event.target.checked)} />我确认对所选国产站点发送版本查询</label>
      <TargetPreview rows={queryTargets} />
    </Modal>
  </div>;
}

function VersionSummary({ summary, loading }: { summary?: DeviceVersionSummary; loading: boolean }) {
  const values = [
    ['设备总数', summary?.total], ['国产配置', summary?.domestic], ['俄供配置', summary?.legacyRussian], ['未配置', summary?.unknown], ['类型不一致', summary?.mismatch], ['查询失败', summary?.failed], ['离线缓存', summary?.offlineCached],
  ] as const;
  return <div className="device-version-summary-area">
    <section className="device-version-summary" aria-label="设备版本汇总">{values.map(([label, value], index) => <div key={label} className={index >= 4 && (value ?? 0) > 0 ? 'has-attention' : ''}><span>{label}</span><strong>{loading && value === undefined ? '—' : (value ?? 0).toLocaleString('zh-CN')}</strong></div>)}</section>
    <section className="lower-software-summary" aria-label="下级软件版本">
      <div className="lower-software-heading"><span>下级软件版本</span><small>按来源去重，不按站点重复计数</small></div>
      <div className="lower-software-list">{loading && !summary ? <span className="text-muted">正在读取…</span> : summary?.lowerSoftwareVersions.length ? summary.lowerSoftwareVersions.map((item) => <div className="lower-software-item" key={`${item.softwareVersion}\u0000${item.buildCommit}`}><strong>{item.softwareVersion || '版本未知'}</strong><code title={item.buildCommit}>{item.buildCommit ? item.buildCommit.slice(0, 12) : '无构建提交'}</code><span>{item.sourceCount.toLocaleString('zh-CN')} 个来源</span></div>) : <span className="text-muted">尚未获取下级软件版本</span>}</div>
    </section>
  </div>;
}

function VersionValue({ value, raw }: { value?: string; raw?: number }) {
  if (!value && raw === undefined) return <span className="text-muted">—</span>;
  return <><strong>{value || '—'}</strong>{raw !== undefined ? <span className="sub-cell version-raw">0x{raw.toString(16).padStart(4, '0').toUpperCase()}</span> : null}</>;
}

function VersionState({ row }: { row: DeviceVersion }) {
  if (!row.versionSupported) return <StatusBadge state="unknown" label="下级版本功能不支持" compact />;
  if (!row.stationRevision) return <StatusBadge state="unknown" label="等待版本状态" compact />;
  if (row.typeMismatch) return <StatusBadge state="bad" label="类型不一致" compact />;
  if (!row.sourceOnline && row.cached) return <StatusBadge state="warning" label="离线缓存" compact />;
  if (row.stale) return <StatusBadge state="warning" label="已过期" compact />;
  const state = row.versionState.toLowerCase();
  if (state.includes('fail') || state.includes('error') || state.includes('timeout')) return <StatusBadge state="bad" label="查询失败" compact />;
  if (row.configuredModuleType === 'legacy_russian') return <StatusBadge state="unknown" label="俄供·不查询" compact />;
  if (row.configuredModuleType === 'unknown') return <StatusBadge state="unknown" label="未配置" compact />;
  if (row.communicationModuleVersion || row.plcVersion) return <StatusBadge state="good" label="版本有效" compact />;
  return <StatusBadge state="unknown" label="等待首查" compact />;
}

function RowOperationStatus({ operation }: { operation: RowOperation }) {
  return <span className={`device-version-operation operation-${operation.state}`}>{operation.state === 'success' ? <CheckCircle2 size={12} aria-hidden="true" /> : operation.state === 'error' ? <XCircle size={12} aria-hidden="true" /> : operation.state === 'running' ? <RefreshCw className="spin" size={12} aria-hidden="true" /> : null}{operation.message}</span>;
}

function TargetPreview({ rows }: { rows: DeviceVersion[] }) {
  return <div className="device-version-targets"><strong>操作目标</strong><ul>{rows.slice(0, 8).map((row) => <li key={versionKey(row)}>{row.sourceName || row.sourceId} · {row.portName || row.portId} · {row.deviceName || row.deviceId}</li>)}</ul>{rows.length > 8 ? <span>另有 {rows.length - 8} 个站点</span> : null}</div>;
}

function moduleTypeLabel(type: ModuleType): string {
  if (type === 'domestic') return '国产';
  if (type === 'legacy_russian') return '俄供';
  return '未配置';
}

function parseModuleType(value: string | null): ModuleType | '' {
  return value === 'domestic' || value === 'legacy_russian' || value === 'unknown' ? value : '';
}

function queryDisabled(row: DeviceVersion): string | undefined {
  if (!row.versionSupported) return '此下级不支持设备版本管理';
  if (!row.stationRevision) return '等待下级返回站点版本状态';
  if (row.configuredModuleType !== 'domestic') return '必须先将站点明确配置为国产模块';
  if (!row.sourceOnline) return '下级来源离线';
  if (!row.portEnabled) return '串口已停用';
  if (!row.portAppliedEnabled) return '串口启用设置尚未应用';
  if (!row.portOpen) return '串口尚未打开';
  if (!row.deviceReadEnabled) return '站点已停读';
  if (!row.appliedDeviceReadEnabled) return '站点恢复读取设置尚未应用';
  if (row.controlPending || row.pendingCommands > 0) return '串口存在待处理控制或命令';
  return undefined;
}

function canQueryVersion(row: DeviceVersion): boolean {
  return !queryDisabled(row);
}

function versionKey(row: DeviceVersion): string {
  return `${row.sourceId}\u0000${row.portId}\u0000${row.deviceId}`;
}

function uniqueVersions(rows: DeviceVersion[]): DeviceVersion[] {
  return [...new Map(rows.map((row) => [versionKey(row), row])).values()];
}

function toggleRow(previous: Map<string, DeviceVersion>, row: DeviceVersion, checked: boolean): Map<string, DeviceVersion> {
  const next = new Map(previous);
  if (checked) next.set(versionKey(row), row);
  else next.delete(versionKey(row));
  return next;
}

function togglePage(previous: Map<string, DeviceVersion>, rows: DeviceVersion[], checked: boolean): Map<string, DeviceVersion> {
  const next = new Map(previous);
  for (const row of rows) {
    if (checked) next.set(versionKey(row), row);
    else next.delete(versionKey(row));
  }
  return next;
}

function findStationVersion(snapshot: AcquisitionSnapshot, row: DeviceVersion) {
  return snapshot.ports.find((port) => port.port_id === row.portId)?.devices.find((device) => device.device_id === row.deviceId);
}

function stationRevisionAdvanced(previous: string, next: string): boolean {
  const canonical = /^[1-9]\d*$/;
  if (!canonical.test(previous) || !canonical.test(next)) return false;
  return next.length > previous.length || (next.length === previous.length && next > previous);
}

export async function runWithConcurrency(jobs: Array<() => Promise<void>>, concurrency: number): Promise<void> {
  let cursor = 0;
  const worker = async () => {
    while (cursor < jobs.length) {
      const index = cursor;
      cursor += 1;
      await jobs[index]();
    }
  };
  await Promise.all(Array.from({ length: Math.min(Math.max(1, concurrency), jobs.length) }, worker));
}

function exportVersionCsv(rows: DeviceVersion[]) {
  const headings = ['下级来源', 'RCD版本', '构建提交', '串口', '站点', '配置类型', '实测类型', '通信模块版本', '通信模块原始值', 'PLC版本', 'PLC原始值', '状态', '最后查询', '最后成功', '错误'];
  const records = rows.map((row) => [row.sourceId, row.rcdSoftwareVersion, row.rcdBuildCommit, row.portId, row.deviceName || row.deviceId, moduleTypeLabel(row.configuredModuleType), moduleTypeLabel(row.detectedModuleType), row.communicationModuleVersion, rawVersion(row.communicationModuleVersionRaw), row.plcVersion, rawVersion(row.plcVersionRaw), row.versionState, row.lastVersionAttemptAt, row.lastVersionSuccessAt, row.lastVersionError]);
  const csv = `\uFEFF${[headings, ...records].map((record) => record.map(csvCell).join(',')).join('\r\n')}`;
  const url = URL.createObjectURL(new Blob([csv], { type: 'text/csv;charset=utf-8' }));
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = `rcd-device-versions-${new Date().toISOString().slice(0, 10)}.csv`;
  anchor.click();
  URL.revokeObjectURL(url);
}

function rawVersion(value?: number): string {
  return value === undefined ? '' : `0x${value.toString(16).padStart(4, '0').toUpperCase()}`;
}

function csvCell(value: unknown): string {
  return `"${String(value ?? '').replaceAll('"', '""')}"`;
}
