import { useEffect, useRef } from 'react';
import { useMutation } from '@tanstack/react-query';
import { api, apiErrorMessage } from '../../api/client';
import type { Source } from '../../api/types';
import { InlineNotice } from '../../components/InlineNotice';
import { LoadingState } from '../../components/PageState';
import { Modal } from '../../components/Modal';
import { probeStageLabel } from '../../lib/labels';

export function SourceDiagnosticsModal({ source, mode, onClose }: { source?: Source; mode?: 'probe' | 'conflicts'; onClose: () => void }) {
  const probe = useMutation({ mutationFn: (id: string) => api.probeSource(id) });
  const lastProbeKey = useRef('');
  useEffect(() => {
    const key = source && mode === 'probe' ? source.id : '';
    if (!key) {
      lastProbeKey.current = '';
      return;
    }
    if (lastProbeKey.current === key) return;
    lastProbeKey.current = key;
    probe.mutate(key);
    // A new modal invocation is keyed by source/mode. The mutation object is
    // intentionally not a dependency because it is recreated by the hook.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [source?.id, mode]);

  const result = probe.data;
  return <Modal open={Boolean(source && mode)} title={mode === 'probe' ? `连接测试 · ${source?.id ?? ''}` : `目录冲突 · ${source?.id ?? ''}`}
    description={source?.endpoint} onClose={onClose} width="large" footer={<button type="button" className="button button-secondary" onClick={onClose}>关闭</button>}>
    {mode === 'probe' ? probe.isPending ? <LoadingState label="正在发现 Endpoint、登录并浏览点表…" /> : probe.error ? <InlineNotice tone="error"><p>{apiErrorMessage(probe.error)}</p></InlineNotice> : result ? <div className="diagnostic-result">
      <InlineNotice tone={result.ok ? 'info' : 'error'}><p><strong>{result.ok ? '连接测试通过' : '连接测试失败'}</strong>{result.error ? `：${result.error}` : `，发现 ${result.variableCount} 个变量和 ${result.methodCount} 个方法。`}</p></InlineNotice>
      <dl className="settings-list compact-settings-list"><div><dt>Endpoint 数量</dt><dd>{result.endpointCount}</dd></div><div><dt>Namespace 数量</dt><dd>{result.namespaceCount}</dd></div><div><dt>总耗时</dt><dd>{result.durationMs} ms</dd></div><div><dt>测试时间</dt><dd>{new Date(result.testedAt).toLocaleString('zh-CN', { hour12: false })}</dd></div></dl>
      <ol className="probe-stages">{result.stages.map((stage) => <li key={stage.name} className={stage.ok ? 'probe-ok' : 'probe-bad'}><span>{stage.ok ? '✓' : '×'}</span><div><strong>{probeStageLabel(stage.name)}</strong><small>{stage.durationMs} ms{stage.message ? ` · ${stage.message}` : ''}</small></div></li>)}</ol>
    </div> : null : null}
    {mode === 'conflicts' ? (source?.catalogConflicts?.length ?? 0) === 0 ? <InlineNotice><p>该来源当前没有目录冲突。</p></InlineNotice> : <div className="conflict-list">{source?.catalogConflicts?.map((conflict, index) => <article key={`${conflict.kind}:${conflict.identifier}:${index}`}><strong>{conflict.identifier}</strong><span>{conflict.kind} · {conflict.groupId || '跨组'} · {conflict.sourceIds.join(', ')}</span><p>{conflict.message}</p></article>)}</div> : null}
  </Modal>;
}
