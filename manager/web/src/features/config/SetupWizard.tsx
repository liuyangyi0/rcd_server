import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api, apiErrorMessage } from '../../api/client';
import { queryKeys } from '../../api/queries';
import { InlineNotice } from '../../components/InlineNotice';
import { Modal } from '../../components/Modal';

export function SetupWizard({ open, onClose, onComplete }: { open: boolean; onClose: () => void; onComplete: (message: string) => void }) {
  const client = useQueryClient();
  const [groupId, setGroupId] = useState('');
  const [groupName, setGroupName] = useState('');
  const [sourceId, setSourceId] = useState('');
  const [sourceName, setSourceName] = useState('');
  const [endpoint, setEndpoint] = useState('opc.tcp://127.0.0.1:4841');
  const [secure, setSecure] = useState(false);
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [groupCreated, setGroupCreated] = useState(false);

  const mutation = useMutation({
    mutationFn: async () => {
      if (!groupCreated) {
        await api.createGroup({ id: groupId.trim(), name: groupName.trim() || groupId.trim() });
        setGroupCreated(true);
      }
      return api.createSource({ id: sourceId.trim(), name: sourceName.trim() || sourceId.trim(), groupId: groupId.trim(), endpoint: endpoint.trim(),
        securityPolicy: secure ? 'Basic256Sha256' : 'None', securityMode: secure ? 'SignAndEncrypt' : 'None', username: username.trim() || undefined,
        password: password || undefined, enabled: true });
    },
    onSuccess: async (source) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
      ]);
      onComplete(`已创建分组 ${groupId.trim()} 和来源 ${source.id}；目录就绪后请设置默认源`);
      setGroupCreated(false);
      onClose();
    },
  });

  return <Modal open={open} title="首次配置向导" description="创建第一个分组和下级 OPC 来源。" onClose={onClose} width="large" footer={<>
    <button type="button" className="button button-secondary" onClick={onClose}>取消</button>
    <button type="submit" form="setup-wizard-form" className="button button-primary" disabled={mutation.isPending || !groupId.trim() || !sourceId.trim() || !endpoint.trim()}>{mutation.isPending ? '正在创建…' : '创建并开始连接'}</button>
  </>}>
    <form id="setup-wizard-form" className="form-grid" onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
      <div className="field-full form-section-title">1. 分组</div>
      <label className="field"><span>Group ID</span><input required disabled={groupCreated} value={groupId} onChange={(event) => setGroupId(event.target.value)} placeholder="line-a" /></label>
      <label className="field"><span>分组名称</span><input disabled={groupCreated} value={groupName} onChange={(event) => setGroupName(event.target.value)} placeholder="一号线" /></label>
      {groupCreated ? <div className="field-full"><InlineNotice><p>分组已创建。若来源连接参数有误，可修正后直接重试，不会重复创建分组。</p></InlineNotice></div> : null}
      <div className="field-full form-section-title">2. 下级 OPC</div>
      <label className="field"><span>Source ID</span><input required value={sourceId} onChange={(event) => setSourceId(event.target.value)} placeholder="line-a-primary" /></label>
      <label className="field"><span>来源名称</span><input value={sourceName} onChange={(event) => setSourceName(event.target.value)} /></label>
      <label className="field field-full"><span>Endpoint</span><input required value={endpoint} onChange={(event) => setEndpoint(event.target.value)} /></label>
      <label className="confirm-check field-full"><input type="checkbox" checked={secure} onChange={(event) => setSecure(event.target.checked)} /><span className="checkbox-visual" /><span>使用 Basic256Sha256 / SignAndEncrypt</span></label>
      {secure ? <><label className="field"><span>用户名（留空为匿名）</span><input value={username} onChange={(event) => setUsername(event.target.value)} /></label><label className="field"><span>密码</span><input type="password" autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} /></label></> : null}
      <div className="field-full"><InlineNotice><p>来源创建后会自动执行连接和目录发现。只有目录进入 Ready 后才能设置默认源。</p></InlineNotice></div>
      {mutation.error ? <div className="field-full"><InlineNotice tone="error"><p>{apiErrorMessage(mutation.error)}{groupCreated ? '；分组已保留，请修正来源配置后重试。' : ''}</p></InlineNotice></div> : null}
    </form>
  </Modal>;
}
