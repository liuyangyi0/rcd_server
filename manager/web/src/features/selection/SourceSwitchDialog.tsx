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

export function SourceSwitchDialog({ open, mode, group, sources, sessions, initialSessionId, onClose, onSuccess }: SourceSwitchDialogProps) {
  const [sessionId, setSessionId] = useState('');
  const [sourceId, setSourceId] = useState('');
  const [confirmed, setConfirmed] = useState(false);
  const wasOpen = useRef(false);
  const sessionMutation = useSelectSessionSource();
  const defaultMutation = useSetDefaultSource();

  const session = useMemo(() => sessions.find((item) => item.id === sessionId), [sessionId, sessions]);
  const currentSourceId = mode === 'default' ? group.defaultSourceId : (session?.selectedSources[group.id] ?? group.defaultSourceId);
  const currentSource = sources.find((source) => source.id === currentSourceId);
  const mutationError = sessionMutation.error ?? defaultMutation.error;
  const pending = sessionMutation.isPending || defaultMutation.isPending;

  const sourceSelectable = (source: Source) => source.enabled && ['ready', 'cached'].includes(source.catalogState.toLowerCase());
  const preferredSourceId = (currentId: string) => sources.find((source) => source.id !== currentId && sourceSelectable(source) && sourceHealth(source) === 'good')?.id ?? currentId;

  useEffect(() => {
    if (open && !wasOpen.current) {
      const nextSessionId = mode === 'session' ? (initialSessionId ?? sessions[0]?.id ?? '') : '';
      setSessionId(nextSessionId);
      const selectedSession = sessions.find((item) => item.id === nextSessionId);
      const currentId = mode === 'default' ? group.defaultSourceId : (selectedSession?.selectedSources[group.id] ?? group.defaultSourceId);
      setSourceId(preferredSourceId(currentId));
      setConfirmed(false);
      sessionMutation.reset();
      defaultMutation.reset();
    }
    wasOpen.current = open;
  }, [group.defaultSourceId, group.id, initialSessionId, mode, open, sessions]); // mutation instances are stable

  const handleSubmit = async () => {
    if (!confirmed || (mode === 'session' && !sourceId)) return;
    if (mode === 'session') {
      if (!session) return;
      await sessionMutation.mutateAsync({
        sessionId: session.id,
        groupId: group.id,
        input: { sourceId, revision: session.routeRevisions[group.id] ?? 1 },
      });
      onSuccess?.(`会话 ${shortSessionId(session.id)} 已切换至 ${sourceId}`);
    } else {
      await defaultMutation.mutateAsync({ groupId: group.id, input: { sourceId, revision: group.selectionRevision } });
      onSuccess?.(`分组 ${group.id} 的默认数据源已设为 ${sourceId}`);
    }
    onClose();
  };

  const unavailable = mode === 'session' && !session;
  const unchanged = sourceId === currentSourceId;
  const selectedSource = sources.find((source) => source.id === sourceId);
  const selectedUnavailable = Boolean(selectedSource && !sourceSelectable(selectedSource));

  return (
    <Modal
      open={open}
      title={mode === 'default' ? '设置默认数据源' : '切换数据源'}
      description={`分组 ${group.id}`}
      onClose={onClose}
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>取消</button>
          <button className="button button-primary" type="button" disabled={pending || unavailable || selectedUnavailable || !confirmed || (mode === 'session' && !sourceId) || unchanged} onClick={() => void handleSubmit()}>
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
                    {source.qualityKnown ? `${formatNumber(source.goodPoints)} / ${formatNumber(source.totalPoints)}` : `${formatNumber(source.totalPoints)} 点`} {selectable ? (source.lastError || (source.qualityKnown && health === 'good' ? 'Good' : '')) : `不可选 · ${source.catalogState}`}
                  </span>
                </span>
                <span className="source-option-endpoint">{source.endpoint}</span>
              </span>
            </label>
          );
        })}
      </fieldset>

      <InlineNotice>
        <p>切换后，该会话的业务点立即从新选中源的最新缓存发布，并保留该源的 OPC UA 质量与源时间戳。</p>
      </InlineNotice>
      <InlineNotice tone="warning">
        <p>执行中的命令固定在原始下级，直至完成或超时；存在待处理命令时，服务会拒绝切换。</p>
      </InlineNotice>
      {mode === 'session' && !session ? <InlineNotice tone="warning"><p>原目标 Session 已结束，本次操作已禁用；不会自动切换到其他 Session。</p></InlineNotice> : null}

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
