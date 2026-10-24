import { useRef, useState } from 'react';
import { Activity, Download, PackageOpen, RefreshCw, ShieldAlert, Upload } from 'lucide-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api, apiErrorMessage } from '../api/client';
import type { ConfigurationDocument, ConfigurationImportResult } from '../api/types';
import { queryKeys, useGroups, useOverview, useSessions, useSystemInfo } from '../api/queries';
import { EmptyState, ErrorState, LoadingState } from '../components/PageState';
import { PageHeader } from '../components/PageHeader';
import { InlineNotice } from '../components/InlineNotice';
import { CopyTextButton } from '../components/CopyTextButton';
import { formatDateTime, shortClientName, shortSessionId } from '../lib/format';
import { SoftwareVersions } from '../features/operations/SoftwareVersions';
import { FleetStatus } from '../features/operations/FleetStatus';

function formatBytes(value: number): string {
  if (value < 0) return '不可用';
  if (value === 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const index = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

export default function SettingsPage() {
  const client = useQueryClient();
  const overview = useOverview();
  const groups = useGroups();
  const sessions = useSessions();
  const system = useSystemInfo();
  const fileInput = useRef<HTMLInputElement>(null);
  const [importDocument, setImportDocument] = useState<ConfigurationDocument>();
  const [importFileName, setImportFileName] = useState('');
  const [importMessage, setImportMessage] = useState('');

  const importMutation = useMutation({
    mutationFn: ({ document, dryRun }: { document: ConfigurationDocument; dryRun: boolean }) => api.importConfiguration(document, dryRun),
    onSuccess: async (result, variables) => {
      setImportMessage(importSummary(result, variables.dryRun));
      if (!variables.dryRun) {
        await Promise.all([
          client.invalidateQueries({ queryKey: queryKeys.groups }),
          client.invalidateQueries({ queryKey: queryKeys.allSources }),
          client.invalidateQueries({ queryKey: queryKeys.overview }),
          client.invalidateQueries({ queryKey: queryKeys.allManagementAudit }),
        ]);
      }
    },
  });

  const loading = overview.isLoading || sessions.isLoading || groups.isLoading || system.isLoading;
  const error = overview.error ?? sessions.error ?? groups.error ?? system.error;
  const info = system.data;
  const canImport = (groups.data?.length ?? 0) === 0;
  const webPort = window.location.port || (window.location.protocol === 'https:' ? '443' : '80');

  const chooseConfiguration = async (file?: File) => {
    setImportMessage('');
    importMutation.reset();
    if (!file) return;
    try {
      const parsed = JSON.parse(await file.text()) as ConfigurationDocument;
      setImportDocument(parsed);
      setImportFileName(file.name);
      importMutation.mutate({ document: parsed, dryRun: true });
    } catch (fileError) {
      setImportDocument(undefined);
      setImportFileName(file.name);
      setImportMessage(`无法读取配置文件：${fileError instanceof Error ? fileError.message : 'JSON 格式错误'}`);
    }
  };

  return (
    <div className="page settings-page">
      <PageHeader title="系统信息与维护" description="查看运行资源、证书有效期，设置审计操作者并执行备份或诊断。" actions={<button className="button button-secondary" type="button" onClick={() => void Promise.all([overview.refetch(), groups.refetch(), sessions.refetch(), system.refetch()])}><RefreshCw aria-hidden="true" size={16} />刷新</button>} />

      {loading ? <LoadingState /> : error ? <ErrorState error={error} onRetry={() => void Promise.all([overview.refetch(), groups.refetch(), sessions.refetch(), system.refetch()])} /> : (
        <div className="settings-layout">
          <SoftwareVersions />
          <FleetStatus />
          <section className="settings-section">
            <h2>服务接口</h2>
            <dl className="settings-list">
              <div><dt>OPC UA Endpoint</dt><dd>{info?.opcEndpoint || overview.data?.opcEndpoint || '—'}</dd></div>
              <div><dt>Web 管理地址</dt><dd>{window.location.origin}</dd></div>
              <div><dt>Web 监听</dt><dd>{info?.httpListen || '—'}</dd></div>
              <div><dt>应用 URI</dt><dd>urn:RcdManager:rcd_manager</dd></div>
              <div><dt>业务命名空间</dt><dd>ns=2 · urn:SerialServer</dd></div>
              <div><dt>管理命名空间</dt><dd>ns=3 · urn:RcdManager:management</dd></div>
              <div><dt>版本</dt><dd>{overview.data?.version ?? '—'} {overview.data?.gitSha && overview.data.gitSha !== '—' ? `(${overview.data.gitSha})` : ''}</dd></div>
            </dl>
          </section>

          <section className="settings-section">
            <h2>运行参数</h2>
            <dl className="settings-list">
              <div><dt>采样 / 发布</dt><dd>{info?.samplingIntervalMs ?? '—'} / {info?.publishingIntervalMs ?? '—'} ms</dd></div>
              <div><dt>命令 / Pending 超时</dt><dd>{info?.commandTimeoutMs ?? '—'} / {info?.pendingTimeoutMs ?? '—'} ms</dd></div>
              <div><dt>容量限制</dt><dd>{info?.maxSources ?? '—'} 个来源 / {info?.maxSessions ?? '—'} 个 Session</dd></div>
              <div><dt>Goroutine</dt><dd>{info?.metrics.goroutines ?? '—'}</dd></div>
              <div><dt>已分配内存</dt><dd>{info ? formatBytes(info.metrics.allocatedBytes) : '—'}</dd></div>
              <div><dt>刷新队列</dt><dd>{info?.metrics.refreshQueueDepth ?? '—'}</dd></div>
              <div><dt>待持久化（目录 / 命令 / 审计 / 状态 / 告警）</dt><dd>{info ? `${info.metrics.dirtyCatalogs} / ${info.metrics.dirtyCommands} / ${info.metrics.dirtyAudits} / ${info.metrics.dirtyStatusEvents} / ${info.metrics.dirtyStatusAlarms}` : '—'}</dd></div>
              <div><dt>SSE 客户端 / 丢弃事件</dt><dd>{info?.metrics.eventSubscribers ?? '—'} / {info?.metrics.droppedEventNotifications ?? '—'}</dd></div>
            </dl>
            <div className="settings-actions"><a className="button button-outline" href="/metrics" target="_blank" rel="noreferrer"><Activity size={16} />打开 Prometheus 指标</a></div>
          </section>

          <section className="settings-section">
            <h2>证书（只读）</h2>
            {info?.certificate.error ? <InlineNotice tone="error"><p>无法读取服务证书：{info.certificate.error}</p></InlineNotice> : null}
            {info?.certificate.expiryWarning ? <InlineNotice tone="warning"><p>证书将在 {info.certificate.daysRemaining} 天后到期，请安排维护窗口更换。</p></InlineNotice> : null}
            <dl className="settings-list">
              <div><dt>Subject</dt><dd>{info?.certificate.subject || '—'}</dd></div>
              <div><dt>有效期</dt><dd>{formatDateTime(info?.certificate.notBefore)} 至 {formatDateTime(info?.certificate.notAfter)}</dd></div>
              <div><dt>剩余天数</dt><dd className={info?.certificate.expiryWarning ? 'text-warning' : 'text-good'}>{info?.certificate.daysRemaining ?? '—'}</dd></div>
              <div><dt>SHA-256 指纹</dt><dd><code className="fingerprint">{info?.certificate.sha256 || '—'}</code></dd></div>
              <div><dt>Application URI</dt><dd>{info?.certificate.applicationUris.join(', ') || '—'}</dd></div>
            </dl>
          </section>

          <section className="settings-section">
            <h2>存储与保留</h2>
            <dl className="settings-list">
              <div><dt>数据目录</dt><dd>{info?.dataDirectory || '—'}<span className="sub-cell">{info ? formatBytes(info.storage.dataDirectoryBytes) : '—'}</span></dd></div>
              <div><dt>SQLite</dt><dd>{info?.databasePath || '—'}<span className="sub-cell">{info ? formatBytes(info.storage.databaseBytes) : '—'}</span></dd></div>
              <div><dt>日志目录</dt><dd>{info?.logDirectory || '—'}<span className="sub-cell">{info ? formatBytes(info.storage.logBytes) : '—'}</span></dd></div>
              <div><dt>磁盘剩余</dt><dd>{info ? formatBytes(info.storage.diskFreeBytes) : '—'}</dd></div>
              <div><dt>审计保留</dt><dd>{info?.storage.auditRetentionDays ?? '—'} 天，最多 {info?.storage.maxAuditRows.toLocaleString('zh-CN') ?? '—'} 条/表</dd></div>
            </dl>
          </section>

          <section className="settings-section">
            <h2>操作者标识</h2>
            <p className="section-description">管理审计自动记录已登录账号与客户端 IP。修改密码或退出登录请使用页面右上角入口。</p>
          </section>

          <section className="settings-section">
            <h2>备份与诊断</h2>
            <p className="section-description">配置备份不包含密码；诊断包不包含数据库或密码，只含运行摘要、近期审计和日志尾部。</p>
            <div className="settings-action-grid">
              <a className="button button-outline" href="/api/v1/configuration"><Download size={16} />导出配置 JSON</a>
              <button className="button button-secondary" type="button" onClick={() => fileInput.current?.click()}><Upload size={16} />选择配置并预检</button>
              <a className="button button-secondary" href="/api/v1/diagnostics"><PackageOpen size={16} />下载诊断包</a>
              <input ref={fileInput} className="visually-hidden" type="file" accept="application/json,.json" onChange={(event) => void chooseConfiguration(event.target.files?.[0])} />
            </div>
            {importFileName ? <p className="selected-file">已选择：{importFileName}</p> : null}
            {importMutation.isPending ? <p className="settings-feedback">正在校验配置文件…</p> : null}
            {importMutation.error ? <InlineNotice tone="error"><p>{apiErrorMessage(importMutation.error)}</p></InlineNotice> : null}
            {importMessage ? <InlineNotice tone={importMessage.startsWith('无法') ? 'error' : 'info'}><p>{importMessage}</p></InlineNotice> : null}
            {importDocument ? <button className="button button-primary" type="button" disabled={!canImport || importMutation.isPending || !importMutation.data?.dryRun} onClick={() => importMutation.mutate({ document: importDocument, dryRun: false })}><Upload size={16} />导入到空管理器</button> : null}
            {!canImport && importDocument ? <p className="field-error">当前已有分组。为避免覆盖在线路由，只允许预检；实际恢复请在空配置管理器上执行。</p> : null}
          </section>

          <section className="settings-section settings-wide">
            <h2>安全边界（保持当前配置）</h2>
            <InlineNotice tone="warning"><p><strong>Web 管理接口需要登录。</strong> 当前 HTTP 不加密传输，OPC 选源与旧命令接口仍允许匿名控制，下级日志推送独立接入；仍须部署在受信任的隔离内网。</p></InlineNotice>
            <div className="security-callout"><ShieldAlert aria-hidden="true" size={30} /><div><strong>部署前检查</strong><p>通过 Windows 防火墙或 VLAN 仅允许上级 OPC、运维终端和指定管理网段访问 4840/{webPort} 端口。</p></div></div>
            <dl className="settings-list compact-settings-list"><div><dt>None/None</dt><dd>Anonymous</dd></div><div><dt>Basic256Sha256</dt><dd>SignAndEncrypt · Anonymous / Username</dd></div><div><dt>跨域写入</dt><dd>拒绝；仅允许同源浏览器请求</dd></div></dl>
          </section>

          <section className="settings-section settings-wide">
            <h2>在线上级 Session</h2>
            {(sessions.data ?? []).length === 0 ? <EmptyState title="暂无在线 Session" description="上级 OPC 客户端激活 Session 后会显示在这里。" /> : (
              <div className="table-scroll"><table className="data-table responsive-table"><thead><tr><th>Session ID</th><th>客户端</th><th>远端地址</th><th>激活时间</th><th>最后活动</th><th>已选路由</th></tr></thead><tbody>{(sessions.data ?? []).map((session) => <tr key={session.id}><td data-label="Session ID"><strong title={session.id}>{shortSessionId(session.id)}</strong><CopyTextButton value={session.id} label="复制 Session ID" /></td><td data-label="客户端" title={session.clientName}>{shortClientName(session.clientName)}</td><td data-label="远端地址">{session.remoteAddress}</td><td data-label="激活时间">{formatDateTime(session.connectedAt)}</td><td data-label="最后活动">{formatDateTime(session.lastSeenAt)}</td><td data-label="已选路由">{Object.keys(session.selectedSources).length} / {(groups.data ?? []).length}</td></tr>)}</tbody></table></div>
            )}
          </section>
        </div>
      )}
    </div>
  );
}

function importSummary(result: ConfigurationImportResult, dryRun: boolean): string {
  const prefix = dryRun ? '预检通过' : '配置导入完成';
  const counts = dryRun && result.updatedGroups > 0
    ? `文件包含 ${result.updatedGroups} 个分组、${result.updatedSources} 个来源；当前实例仅允许预检。`
    : `${result.createdGroups} 个分组、${result.createdSources} 个来源。`;
  return `${prefix}：${counts}${result.warnings.length ? ` 提示：${result.warnings.join('；')}` : ''}`;
}
