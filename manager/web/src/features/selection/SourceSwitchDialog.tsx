import { useEffect, useMemo, useRef, useState } from 'react';
import { Check, Info } from 'lucide-react';
import type { Group, Source, UaSession } from '../../api/types';
import { apiErrorMessage } from '../../api/client';
import { useSelectSessionSource, useSetDefaultSource } from '../../api/queries';
import { Modal } from '../../components/Modal';
import { InlineNotice } from '../../components/InlineNotice';
import { StatusBadge } from '../../components/StatusBadge';
import { formatNumber, shortSessionId } from '../../lib/format';
import { sourceHealth, statusLabel } from '../../lib/status';
import { groupDeviceLabel, sourceHasGroup } from '../../lib/sourceGroups';

interface SourceSwitchDialogProps {
  open: boolean;
  mode: 'session' | 'default';
  group: Group;
  sources: Source[];
  sessions: UaSession[];
  initialSessionId?: string;
  onClose: () => void;
  onSuccess?: (message: string) => void;
}

export function SourceSwitchDialog({ open, mode, group, sources: allSources, sessions, initialSessionId, onClose, onSuccess }: SourceSwitchDialogProps) {
  const sources = useMemo(() => allSources.filter((source) => sourceHasGroup(source, group.id)), [allSources, group.id]);
  const [sessionId, setSessionId] = useState('');
  const [sourceId, setSourceId] = useState('');
  const [confirmed, setConfirmed] = useState(false);
  const [editRevision, setEditRevision] = useState(0);
  const wasOpen = useRef(false);
  const sessionMutation = useSelectSessionSource();
  const defaultMutation = useSetDefaultSource();

  const session = useMemo(() => sessions.find((item) => item.id === sessionId), [sessionId, sessions]);
  const currentSourceId = mode === 'default' ? group.defaultSourceId : (session?.selectedSources[group.id] ?? group.defaultSourceId);
  const currentSource = sources.find((source) => source.id === currentSourceId);
  const mutationError = sessionMutation.error ?? defaultMutation.error;
  const pending = sessionMutation.isPending || defaultMutation.isPending;

  const sourceSelectable = (source: Source) => source.enabled && ['ready', 'cached'].includes(source.catalogState.toLowerCase()) && (source.availableGroupIds === undefined || source.availableGroupIds.includes(group.id));
  const preferredSourceId = (currentId: string) => sources.find((source) => source.id !== currentId && sourceSelectable(source) && sourceHealth(source) === 'good')?.id ?? currentId;

  useEffect(() => {
    if (open && !wasOpen.current) {
      const nextSessionId = mode === 'session' ? (initialSessionId ?? sessions[0]?.id ?? '') : '';
      setSessionId(nextSessionId);
      const selectedSession = sessions.find((item) => item.id === nextSessionId);
      const currentId = mode === 'default' ? group.defaultSourceId : (selectedSession?.selectedSources[group.id] ?? group.defaultSourceId);
      setEditRevision(mode === 'default' ? group.selectionRevision : (selectedSession?.routeRevisions[group.id] ?? 1));
      setSourceId(preferredSourceId(currentId));
      setConfirmed(false);
      sessionMutation.reset();
      defaultMutation.reset();
    }
    wasOpen.current = open;
  }, [group.defaultSourceId, group.id, group.selectionRevision, initialSessionId, mode, open, sessions]); // mutation instances are stable

  const handleSubmit = async () => {
    if (!confirmed || (mode === 'session' && !sourceId)) return;
    if (mode === 'session') {
      if (!session) return;
      await sessionMutation.mutateAsync({
        sessionId: session.id,
        groupId: group.id,
        input: { sourceId, revision: editRevision },
      });
      onSuccess?.(`会话 ${shortSessionId(session.id)} 的整逻辑组 ${group.id} 已切换至 ${sourceId}；其他组与 Session 不变`);
    } else {
      await defaultMutation.mutateAsync({ groupId: group.id, input: { sourceId, revision: editRevision } });
      onSuccess?.(`分组 ${group.id} 的默认数据源已设为 ${sourceId}`);
    }
    onClose();
  };

  const unavailable = mode === 'session' && !session;
  const unchanged = sourceId === currentSourceId;
  const selectedSource = sources.find((source) => source.id === sourceId);
  const selectedUnavailable = Boolean(sourceId && (!selectedSource || !sourceSelectable(selectedSource)));
  const currentRevision = mode === 'default' ? group.selectionRevision : (session?.routeRevisions[group.id] ?? 1);
  const staleSelection = editRevision !== 0 && currentRevision !== editRevision;

  return (
    <Modal
      open={open}
      title={mode === 'default' ? '设置逻辑组默认数据源' : '切换整逻辑设备组'}
      description={`逻辑组 ${group.id} · ${formatNumber(group.totalPoints)} 个并集点位`}
      onClose={onClose}
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>取消</button>
          <button className="button button-primary" type="button" disabled={pending || staleSelection || group.pendingCommandCount > 0 || unavailable || selectedUnavailable || !confirmed || (mode === 'session' && !sourceId) || unchanged} onClick={() => { void handleSubmit().catch(() => { /* Mutation state renders the actionable error below. */ }); }}>
            {pending ? '正在提交…' : mode === 'default' ? '确认设置' : '确认切换'}
          </button>
        </>
      }
    >
      <div className="selection-section">
        {mode === 'session' ? <>
          <span className="field-label">目标上级 Session</span>
          <div className="current-source-box">
            <strong>{session ? `${session.clientName || '匿名客户端'} · ${shortSessionId(session.id)}` : '目标 Session 已结束'}</strong>
            <span>{session?.remoteAddress ?? '请关闭弹窗并重新选择在线 Session。'}</span>
          </div>
        </> : null}
        <span className="field-label">当前{mode === 'default' ? '默认' : '选中'}源</span>
        <div className="current-source-box">
          <StatusBadge state={sourceHealth(currentSource)} label={`${currentSourceId || '未设置'}（${statusLabel(sourceHealth(currentSource))}）`} />
          <span>{currentSource?.endpoint ?? '未发现该来源'}</span>
        </div>
      </div>

      <fieldset className="source-options" disabled={pending || unavailable}>
        <legend>新的数据源</legend>
        {mode === 'default' ? (
          <label className={`source-option${sourceId === '' ? ' is-selected' : ''}`}>
            <input type="radio" name="source" value="" checked={sourceId === ''} onChange={() => { setSourceId(''); setConfirmed(false); }} />
            <span className="radio-visual">{sourceId === '' ? <span /> : null}</span>
            <span className="source-option-content">
              <span className="source-option-main"><strong>未设置默认源</strong><span className="text-muted">新 Session 将不自动选择来源</span></span>
            </span>
          </label>
        ) : null}
        {sources.length === 0 ? <div className="source-empty">该分组尚未配置下级 OPC 来源</div> : sources.map((source) => {
          const health = sourceHealth(source);
          const selected = source.id === sourceId;
          const selectable = sourceSelectable(source);
          return (
            <label key={source.id} className={`source-option${selected ? ' is-selected' : ''}${selectable ? '' : ' is-disabled'}`}>
              <input type="radio" name="source" value={source.id} checked={selected} disabled={!selectable} onChange={() => { setSourceId(source.id); setConfirmed(false); }} />
              <span className="radio-visual">{selected ? <span /> : null}</span>
              <span className="source-option-content">
                <span className="source-option-main">
                  <strong>{source.id}</strong>
                  <StatusBadge state={health} label={statusLabel(health)} compact />
                  <span className={health === 'bad' ? 'text-bad' : source.qualityKnown ? 'text-good' : 'text-muted'}>
                    来源整体 {source.qualityKnown ? `${formatNumber(source.goodPoints)} / ${formatNumber(source.totalPoints)}` : `${formatNumber(source.totalPoints)} 点`} {selectable ? (source.lastError || (source.qualityKnown && health === 'good' ? 'Good' : '')) : `不可选 · ${source.availableGroupIds && !source.availableGroupIds.includes(group.id) ? '未发现本组目录' : source.catalogState}`}
                  </span>
                </span>
                <span className="source-option-endpoint">{source.endpoint}</span>
                <span className="source-option-endpoint">本组归属：{groupDeviceLabel(source, group.id)}</span>
              </span>
            </label>
          );
        })}
      </fieldset>

      <InlineNotice>
        <p>{mode === 'session' ? `本次只切换当前目标 Session 的逻辑组 ${group.id}，组内全部点位统一切换；不是单点切换，也不会把该下级提供的其他组一起切换。保留选中源的质量与源时间戳。` : `默认源只影响之后新激活的 Session 在逻辑组 ${group.id} 中的初始选择；现有 Session 和其他组不变。`}</p>
      </InlineNotice>
      <InlineNotice tone="warning">
        <p>执行中的命令固定在原始下级，直至完成或超时；存在待处理命令时，服务会拒绝切换。</p>
      </InlineNotice>
      <InlineNotice><p>单设备计算点随设备归组；跨设备计算保留在默认归属组，不会根据各设备的选源重新混算。离线来源不会触发自动切换。</p></InlineNotice>
      {mode === 'session' && !session ? <InlineNotice tone="warning"><p>原目标 Session 已结束，本次操作已禁用；不会自动切换到其他 Session。</p></InlineNotice> : null}
      {staleSelection && !unavailable ? <InlineNotice tone="warning"><p>该逻辑组的选源版本已变化。请关闭后重新打开弹窗，核对最新路由再提交，不会覆盖其他操作。</p></InlineNotice> : null}

      <label className="confirm-check">
        <input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />
        <span className="checkbox-visual">{confirmed ? <Check aria-hidden="true" size={14} /> : null}</span>
        <span>确认已核对目标下级状态</span>
      </label>

      {unchanged && sourceId ? (
        <div className="modal-inline-message"><Info aria-hidden="true" size={16} />所选来源与当前来源相同，无需提交。</div>
      ) : null}
      {mutationError ? <div className="form-error" role="alert">{apiErrorMessage(mutationError)}</div> : null}
    </Modal>
  );
}
