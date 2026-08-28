import { useMemo, useState } from 'react';
import { ArrowLeftRight, ChartNoAxesColumn, Info, TriangleAlert } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import type { Group, Source, UaSession } from '../../api/types';
import { Drawer } from '../../components/Drawer';
import { InlineNotice } from '../../components/InlineNotice';
import { StatusBadge } from '../../components/StatusBadge';
import { formatNumber, formatRelativeTime, shortSessionId } from '../../lib/format';
import { sourceHealth, statusLabel } from '../../lib/status';
import { SourceSwitchDialog } from './SourceSwitchDialog';

interface GroupDetailDrawerProps {
  group?: Group;
  sources: Source[];
  sessions: UaSession[];
  onClose: () => void;
}

export function GroupDetailDrawer({ group, sources, sessions, onClose }: GroupDetailDrawerProps) {
  const navigate = useNavigate();
  const [switchMode, setSwitchMode] = useState<'session' | 'default'>();
  const [sessionId, setSessionId] = useState('');
  const [success, setSuccess] = useState('');
  const effectiveSessionId = sessionId || sessions[0]?.id || '';
  const session = sessions.find((item) => item.id === effectiveSessionId);
  const groupSources = useMemo(() => sources.filter((source) => source.groupId === group?.id), [group?.id, sources]);
  const selectedSourceId = group ? (session?.selectedSources[group.id] ?? group.defaultSourceId) : '';
  const selectedSource = groupSources.find((source) => source.id === selectedSourceId);
  const health = sourceHealth(selectedSource);
  const deviatesFromDefault = Boolean(group && session && selectedSourceId !== group.defaultSourceId);

  return (
    <>
      <Drawer open={Boolean(group)} title={group?.name || group?.id || ''} onClose={onClose}>
        {group ? (
          <>
            <div className="drawer-group-id">{group.id} · {group.sourceCount} 个来源</div>
            <label className="field drawer-session-field">
              <span>查看上级会话</span>
              <select value={effectiveSessionId} onChange={(event) => setSessionId(event.target.value)} disabled={sessions.length === 0}>
                {sessions.length === 0 ? <option value="">暂无在线会话</option> : sessions.map((item) => (
                  <option key={item.id} value={item.id}>{item.clientName} · {shortSessionId(item.id)}</option>
                ))}
              </select>
            </label>

            <dl className="drawer-summary">
              <div><dt>当前选中源</dt><dd><StatusBadge state={health} label={`${selectedSourceId || '未设置'}（${statusLabel(health)}）`} /></dd></div>
              <div><dt>新会话默认源</dt><dd>{group.defaultSourceId || '未设置'}</dd></div>
              <div><dt>路由关系</dt><dd>{deviatesFromDefault ? <span className="route-deviation">偏离默认</span> : <span className="text-good">与默认一致</span>}</dd></div>
              <div><dt>选源版本</dt><dd>{session?.routeRevisions[group.id] ?? group.selectionRevision}<span className="text-muted"> · {formatRelativeTime(group.lastSelectedAt)}</span></dd></div>
            </dl>

            <h3 className="drawer-section-title">成员状态</h3>
            <div className="member-list">
              {groupSources.map((source) => {
                const sourceState = sourceHealth(source);
                return (
                  <div className="member-card" key={source.id}>
                    <div className="member-title"><StatusBadge state={sourceState} label={`${source.id}${source.id === selectedSourceId ? '（当前）' : sourceState === 'good' ? '（可用）' : ''}`} /></div>
                    <div className="member-endpoint">{source.endpoint}</div>
                    <dl>
                      <div><dt>状态</dt><dd className={sourceState === 'bad' ? 'text-bad' : 'text-good'}>{sourceState === 'good' ? '正常' : source.lastError || source.connectionState}</dd></div>
                      <div><dt>连接 / 会话 / 订阅</dt><dd>{source.connectionState} / {source.sessionState} / {source.subscriptionState}<br /><span>{formatNumber(source.monitoredCount)} 个监控点</span></dd></div>
                      <div><dt>Good / Bad</dt><dd>{source.qualityKnown ? <><span className="text-good">{formatNumber(source.goodPoints)}</span> / <span className={source.badPoints ? 'text-bad' : ''}>{formatNumber(source.badPoints)}</span></> : '实时质量未提供'}<span className="sub-cell">最近数据 {formatRelativeTime(source.lastDataAt)}</span></dd></div>
                    </dl>
                  </div>
                );
              })}
              {groupSources.length === 0 ? <div className="source-empty">该分组暂无来源</div> : null}
            </div>

            <dl className="drawer-metrics">
              <div><dt>点位质量</dt><dd className={(selectedSource?.badPoints ?? 0) > 0 ? 'text-bad' : ''}>{selectedSource?.qualityKnown ? ((selectedSource.badPoints ?? 0) > 0 ? selectedSource.lastError || 'Bad' : 'Good') : '实时质量未提供'}<br /><span>{selectedSource?.qualityKnown ? `${formatNumber(selectedSource.goodPoints)} / ${formatNumber(selectedSource.totalPoints)}` : `${formatNumber(selectedSource?.totalPoints ?? 0)} 个目录点`}</span></dd></div>
              <div><dt>最近更新</dt><dd>{formatRelativeTime(selectedSource?.lastDataAt)}</dd></div>
            </dl>

            <InlineNotice tone="warning"><p>管理程序不会自动切换数据源。选中源异常时将继续发布 Bad 质量，由上层 OPC 或操作员决定何时切换。</p></InlineNotice>
            <InlineNotice><p>执行中的命令固定在原始下级，直至完成或超时；新命令发往切换后的数据源。</p></InlineNotice>
            {success ? <div className="drawer-success"><Info aria-hidden="true" size={16} />{success}</div> : null}

            <div className="drawer-actions">
              <button className="button button-primary button-block" type="button" disabled={sessions.length === 0 || group.pendingCommandCount > 0} onClick={() => setSwitchMode('session')}>
                <ArrowLeftRight aria-hidden="true" size={17} />切换数据源
              </button>
              <button className="button button-outline button-block" type="button" disabled={group.pendingCommandCount > 0} onClick={() => setSwitchMode('default')}>设置新 Session 默认源</button>
              <button className="button button-secondary button-block" type="button" onClick={() => navigate(`/points?group=${encodeURIComponent(group.id)}`)}>
                <ChartNoAxesColumn aria-hidden="true" size={17} />查看点位质量
              </button>
            </div>
            {sessions.length === 0 ? <div className="drawer-hint"><TriangleAlert aria-hidden="true" size={15} />暂无在线上级 Session，不能执行按 Session 切换。</div> : null}
          </>
        ) : null}
      </Drawer>
      {group ? (
        <SourceSwitchDialog
          open={Boolean(switchMode)}
          mode={switchMode ?? 'session'}
          group={group}
          sources={groupSources}
          sessions={sessions}
          initialSessionId={effectiveSessionId}
          onClose={() => setSwitchMode(undefined)}
          onSuccess={setSuccess}
        />
      ) : null}
    </>
  );
}
