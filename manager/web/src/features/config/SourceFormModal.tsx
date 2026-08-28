import { useEffect, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { Group, Source } from '../../api/types';
import { api, apiErrorMessage } from '../../api/client';
import { queryKeys } from '../../api/queries';
import { Modal } from '../../components/Modal';
import { InlineNotice } from '../../components/InlineNotice';

export function SourceFormModal({ open, source, groups, onClose, onSaved }: { open: boolean; source?: Source; groups: Group[]; onClose: () => void; onSaved: (message: string) => void }) {
  const client = useQueryClient();
  const [id, setId] = useState('');
  const [name, setName] = useState('');
  const [groupId, setGroupId] = useState('');
  const [endpoint, setEndpoint] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [clearPassword, setClearPassword] = useState(false);
  const [securityPolicy, setSecurityPolicy] = useState('None');
  const [securityMode, setSecurityMode] = useState('None');
  const [enabled, setEnabled] = useState(true);
  const mutation = useMutation({
    mutationFn: () => {
      const input = {
        id: id.trim(), name: name.trim() || id.trim(), groupId, endpoint: endpoint.trim(), username: username.trim() || undefined,
        password: clearPassword ? '' : (password || undefined), securityPolicy, securityMode, enabled, revision: source?.revision,
      };
      return source ? api.updateSource(source.id, input) : api.createSource(input);
    },
    onSuccess: async (saved) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
      ]);
      onSaved(`来源 ${saved.id} 已${source ? '更新' : '创建'}，目录发现将在后台执行`);
      onClose();
    },
  });

  useEffect(() => {
    if (!open) return;
    setId(source?.id ?? '');
    setName(source?.name ?? '');
    setGroupId(source?.groupId ?? groups[0]?.id ?? '');
    setEndpoint(source?.endpoint ?? 'opc.tcp://');
    setUsername(source?.username ?? '');
    setPassword('');
    setClearPassword(false);
    setSecurityPolicy(source?.securityPolicy ?? 'None');
    setSecurityMode(source?.securityMode ?? 'None');
    setEnabled(source?.enabled ?? true);
    mutation.reset();
  }, [open, source?.id]); // Background query refreshes must not erase in-progress edits.

  const validId = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(id.trim());
  const validEndpoint = validOpcEndpoint(endpoint);
  const canSave = validId && validEndpoint && Boolean(groupId) && !mutation.isPending;

  return (
    <Modal
      open={open}
      title={source ? '编辑下级 OPC' : '添加下级 OPC'}
      description="保存后先校验配置，再热应用连接与目录发现。"
      onClose={onClose}
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>取消</button>
          <button className="button button-primary" type="button" disabled={!canSave} onClick={() => mutation.mutate()}>{mutation.isPending ? '保存中…' : '保存并应用'}</button>
        </>
      }
    >
      <div className="form-grid">
        <label className="field"><span>Source ID</span><input value={id} disabled={Boolean(source)} maxLength={64} placeholder="例如 rcd-a-01" onChange={(event) => setId(event.target.value)} /></label>
        <label className="field"><span>显示名称</span><input value={name} maxLength={80} placeholder="默认与 Source ID 相同" onChange={(event) => setName(event.target.value)} /></label>
        <label className="field"><span>所属分组</span><select value={groupId} onChange={(event) => setGroupId(event.target.value)}>{groups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <label className="field field-full"><span>OPC UA Endpoint</span><input value={endpoint} placeholder="opc.tcp://10.10.1.21:4840/" onChange={(event) => setEndpoint(event.target.value)} /></label>
        <label className="field"><span>安全策略</span><select value={securityPolicy} onChange={(event) => { const value = event.target.value; setSecurityPolicy(value); setSecurityMode(value === 'None' ? 'None' : 'SignAndEncrypt'); }}><option value="None">None</option><option value="Basic256Sha256">Basic256Sha256</option></select></label>
        <label className="field"><span>消息安全模式</span><select value={securityMode} disabled={securityPolicy === 'None'} onChange={(event) => setSecurityMode(event.target.value)}><option value="None">None</option><option value="SignAndEncrypt">SignAndEncrypt</option></select></label>
        <label className="field"><span>用户名（可选）</span><input value={username} autoComplete="off" onChange={(event) => setUsername(event.target.value)} /></label>
        <label className="field"><span>{source ? '新密码（留空不修改）' : '密码（可选）'}</span><input type="password" value={password} disabled={clearPassword} autoComplete="new-password" onChange={(event) => setPassword(event.target.value)} /></label>
        {source?.hasPassword ? <label className="confirm-check field-full"><input type="checkbox" checked={clearPassword} onChange={(event) => { setClearPassword(event.target.checked); if (event.target.checked) setPassword(''); }} /><span className="checkbox-visual" /><span>清除已保存的下级密码</span></label> : null}
        <label className="confirm-check field-full"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /><span className="checkbox-visual" /><span>启用该下级连接</span></label>
      </div>
      {!validEndpoint && endpoint !== 'opc.tcp://' ? <p className="field-error">请输入合法的 opc.tcp Endpoint。</p> : null}
      <InlineNotice><p>凭据由后端使用 Windows DPAPI machine scope 加密，API 不会回传明文密码。</p></InlineNotice>
      {mutation.error ? <div className="form-error" role="alert">{apiErrorMessage(mutation.error)}</div> : null}
    </Modal>
  );
}

export function validOpcEndpoint(value: string): boolean {
  return /^opc\.tcp:\/\/[^\s/?#]+(?::\d+)?(?:\/[^\s?#]*)?$/i.test(value.trim());
}
