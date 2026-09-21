import { useMemo, useState } from 'react';
import { ArrowLeftRight, ChartNoAxesColumn, Info, TriangleAlert } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import type { Group, Source, UaSession } from '../../api/types';
import { Drawer } from '../../components/Drawer';
import { InlineNotice } from '../../components/InlineNotice';
import { StatusBadge } from '../../components/StatusBadge';
import { formatNumber, formatRelativeTime, shortSessionId } from '../../lib/format';
import { sourceHealth, statusLabel } from '../../lib/status';
import { groupDeviceLabel, sourceHasGroup, sourceUnavailableReason } from '../../lib/sourceGroups';
import { SourceSwitchDialog } from './SourceSwitchDialog';

interface GroupDetailDrawerProps {
  group?: Group;
  sources: Source[];
  sessions: UaSession[];
  sessionId: string;
  onSessionChange: (sessionId: string) => void;
  onClose: () => void;
}

export function GroupDetailDrawer({ group, sources, sessions, sessionId, onSessionChange, onClose }: GroupDetailDrawerProps) {
  const navigate = useNavigate();
  const [switchState, setSwitchState] = useState<{ mode: 'session' | 'default'; sessionId: string }>();
  const [success, setSuccess] = useState('');
  const session = sessions.find((item) => item.id === sessionId);
  const sessionEnded = Boolean(sessionId && !session);
  const groupSources = useMemo(() => sources.filter((source) => group && sourceHasGroup(source, group.id)), [group?.id, sources]);
  const selectedSourceId = group && session ? (session.selectedSources[group.id] ?? group.defaultSourceId) : '';
  const selectedSource = groupSources.find((source) => source.id === selectedSourceId);
  const health = sourceHealth(selectedSource);
  const deviatesFromDefault = Boolean(group && session && selectedSourceId !== group.defaultSourceId);

  return (
    <>
      <Drawer open={Boolean(group)} title={group?.name || group?.id || ''} onClose={onClose}>
        {group ? (
          <>
            <div className="drawer-group-id">逻辑组 {group.id} · {group.sourceCount} 个来源 · {formatNumber(group.totalPoints)} 个并集点位</div>
            <label className="field drawer-session-field">
              <span>查看上级会话</span>
              <select value={sessionId} onChange={(event) => onSessionChange(event.target.value)} disabled={sessions.length === 0}>
                <option value="">{sessions.length === 0 ? '暂无在线会话' : '选择在线会话'}</option>
                {sessionEnded ? <option value={sessionId}>{shortSessionId(sessionId)}（已结束）</option> : null}
                {sessions.map((item) => (
                  <option key={item.id} value={item.id}>{item.clientName} · {shortSessionId(item.id)}</option>
                ))}
              </select>
            </label>

            <dl className="drawer-summary">
              <div><dt>当前选中源</dt><dd>{session ? <StatusBadge state={health} label={`${selectedSourceId || '未设置'}（${statusLabel(health)}）`} /> : <span className="text-muted">{sessionEnded ? '目标 Session 已结束' : '尚未选择会话'}</span>}</dd></div>
              <div><dt>新会话默认源</dt><dd>{group.defaultSourceId || '未设置'}</dd></div>
              <div><dt>路由关系</dt><dd>{!session ? '—' : deviatesFromDefault ? <span className="route-deviation">偏离默认</span> : <span className="text-good">与默认一致</span>}</dd></div>
              <div><dt>选源版本</dt><dd>{session ? (session.routeRevisions[group.id] ?? group.selectionRevision) : '—'}<span className="text-muted"> · {formatRelativeTime(group.lastSelectedAt)}</span></dd></div>
            </dl>

            <h3 className="drawer-section-title">成员状态</h3>
            <div className="member-list">
              {groupSources.map((source) => {
                const sourceState = sourceHealth(source);
                const unavailableReason = sourceUnavailableReason(source, group.id);
                return (
                  <div className="member-card" key={source.id}>
                    <div className="member-title"><StatusBadge state={sourceState} label={`${source.id}${source.id === selectedSourceId ? '（当前）' : unavailableReason ? '（不可选）' : '（可用）'}`} /></div>
                    <div className="member-endpoint">{source.endpoint}</div>
                    <div className="member-endpoint">本组设备：{groupDeviceLabel(source, group.id)}</div>
                    <dl>
                      <div><dt>本组目录</dt><dd className={unavailableReason ? 'text-warning' : 'text-good'}>{unavailableReason || '已发现，可选择'}{!unavailableReason && sourceState !== 'good' ? <span className="sub-cell">离线或异常来源仍可手动选择，但不能作为健康自动候选</span> : null}</dd></div>
                      <div><dt>状态</dt><dd className={sourceState === 'bad' ? 'text-bad' : 'text-good'}>{sourceState === 'good' ? '正常' : source.lastError || source.connectionState}</dd></div>
                      <div><dt>连接 / 会话 / 订阅</dt><dd>{source.connectionState} / {source.sessionState} / {source.subscriptionState}<br /><span>{formatNumber(source.monitoredCount)} 个监控点</span></dd></div>
                      <div><dt>来源整体 Good / Bad</dt><dd>{source.qualityKnown ? <><span className="text-good">{formatNumber(source.goodPoints)}</span> / <span className={source.badPoints ? 'text-bad' : ''}>{formatNumber(source.badPoints)}</span></> : '实时质量未提供'}<span className="sub-cell">包括该来源所有组 · 最近数据 {formatRelativeTime(source.lastDataAt)}</span></dd></div>
                    </dl>
                  </div>
                );
              })}
              {groupSources.length === 0 ? <div className="source-empty">该分组暂无来源</div> : null}
            </div>

            <dl className="drawer-metrics">
              <div><dt>本组默认源质量</dt><dd className={group.badPoints > 0 ? 'text-bad' : ''}>{group.qualityKnown ? (group.badPoints > 0 ? '含坏质量点位' : 'Good') : '实时质量未提供'}<br /><span>{group.qualityKnown ? `${formatNumber(group.goodPoints)} / ${formatNumber(group.totalPoints)}` : `${formatNumber(group.totalPoints)} 个目录点`}</span></dd></div>
              <div><dt>最近更新</dt><dd>{formatRelativeTime(selectedSource?.lastDataAt)}</dd></div>
            </dl>

            <InlineNotice tone="warning"><p>自动切源默认关闭，可在组列表的“自动切源 / 差异告警”配置。手动切源会将当前 Session 本组设为手动固定；来源异常仍上报坏质量。</p></InlineNotice>
            <InlineNotice><p>执行中的命令固定在原始下级，直至完成或超时；新命令发往切换后的数据源。</p></InlineNotice>
            <InlineNotice><p>切换范围是当前 Session 的整逻辑组，不是整个下级，也不是单个变量。同一个下级提供的其他组保持原选源；跨设备计算不会按设备选源重新混算。</p></InlineNotice>
            {success ? <div className="drawer-success"><Info aria-hidden="true" size={16} />{success}</div> : null}

            <div className="drawer-actions">
              <button className="button button-primary button-block" type="button" disabled={!session || group.pendingCommandCount > 0} onClick={() => setSwitchState({ mode: 'session', sessionId })}>
                <ArrowLeftRight aria-hidden="true" size={17} />切换当前 Session 的整组来源
              </button>
              <button className="button button-outline button-block" type="button" disabled={group.pendingCommandCount > 0} onClick={() => setSwitchState({ mode: 'default', sessionId })}>设置新 Session 默认源</button>
              <button className="button button-secondary button-block" type="button" onClick={() => navigate(`/points?group=${encodeURIComponent(group.id)}${sessionId ? `&view=session&session=${encodeURIComponent(sessionId)}` : ''}`)}>
                <ChartNoAxesColumn aria-hidden="true" size={17} />查看点位质量
              </button>
            </div>
            {!session ? <div className="drawer-hint"><TriangleAlert aria-hidden="true" size={15} />{sessionEnded ? '原目标 Session 已结束。请选择其他在线会话，不会自动更换目标。' : '请选择在线上级 Session，才能执行按 Session 切换。'}</div> : null}
          </>
        ) : null}
      </Drawer>
      {group && switchState ? (
        <SourceSwitchDialog
          open
          mode={switchState.mode}
          group={group}
          sources={groupSources}
          sessions={sessions}
          initialSessionId={switchState.sessionId}
          onClose={() => setSwitchState(undefined)}
          onSuccess={setSuccess}
        />
      ) : null}
    </>
  );
}
