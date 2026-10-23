import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { DeviceGroupBinding, Group, Source } from '../../api/types';
import { api, apiErrorMessage } from '../../api/client';
import { queryKeys } from '../../api/queries';
import { Modal } from '../../components/Modal';
import { InlineNotice } from '../../components/InlineNotice';

interface Props { open: boolean; source?: Source; groups: Group[]; onClose: () => void; onSaved: (message: string) => void }

export function SourceFormModal(props: Props) {
  return props.open ? <SourceForm key={props.source?.id ?? 'new'} {...props} /> : null;
}

function SourceForm({ source, groups, onClose, onSaved }: Props) {
  const client = useQueryClient();
  const mounted = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const [id, setId] = useState('');
  const [name, setName] = useState('');
  const [groupId, setGroupId] = useState('');
  const [groupBindings, setGroupBindings] = useState<DeviceGroupBinding[]>([]);
  const [editRevision, setEditRevision] = useState<number>();
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
        password: clearPassword ? '' : (password || undefined), securityPolicy, securityMode, enabled, groupBindings, revision: editRevision,
      };
      return source ? api.updateSource(source.id, input) : api.createSource(input);
    },
    onSuccess: async (saved) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: queryKeys.allSources }),
        client.invalidateQueries({ queryKey: queryKeys.groups }),
        client.invalidateQueries({ queryKey: queryKeys.overview }),
        client.invalidateQueries({ queryKey: queryKeys.allTopologies }),
        client.invalidateQueries({ queryKey: queryKeys.allPoints }),
      ]);
      // Closing this editor does not cancel its accepted request. A late
      // completion may refresh data, but must not close a newly opened editor.
      if (!mounted.current) return;
      onSaved(`来源 ${saved.id} 已${source ? '更新' : '创建'}，目录发现将在后台执行`);
      onClose();
    },
  });

  useEffect(() => {
    setId(source?.id ?? '');
    setName(source?.name ?? '');
    setGroupId(source?.groupId ?? groups[0]?.id ?? '');
    setGroupBindings(source?.groupBindings ?? []);
    setEditRevision(source?.revision);
    setEndpoint(source?.endpoint ?? 'opc.tcp://');
    setUsername(source?.username ?? '');
    setPassword('');
    setClearPassword(false);
    setSecurityPolicy(source?.securityPolicy ?? 'None');
    setSecurityMode(source?.securityMode ?? 'None');
    setEnabled(source?.enabled ?? true);
    mutation.reset();
  }, [source?.id]); // Background query refreshes must not erase in-progress edits.

  const validId = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(id.trim());
  const validEndpoint = validOpcEndpoint(endpoint);
  const canSave = validId && validEndpoint && Boolean(groupId) && !mutation.isPending;

  return (
    <Modal
      open
      title={source ? '编辑下级 OPC' : '添加下级 OPC'}
      description="保存后先校验配置，再热应用连接与目录发现。"
      onClose={onClose}
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>{mutation.isPending ? '关闭（请求继续执行）' : '取消'}</button>
          <button className="button button-primary" type="button" disabled={!canSave} onClick={() => mutation.mutate()}>{mutation.isPending ? '保存中…' : '保存并应用'}</button>
        </>
      }
    >
      <div className="form-grid">
        <label className="field"><span>Source ID</span><input value={id} disabled={Boolean(source) || mutation.isPending} maxLength={64} placeholder="例如 rcd-a-01" onChange={(event) => setId(event.target.value)} /></label>
        <label className="field"><span>显示名称</span><input value={name} disabled={mutation.isPending} maxLength={80} placeholder="默认与 Source ID 相同" onChange={(event) => setName(event.target.value)} /></label>
        <label className="field"><span>默认归属组（未映射点）</span><select value={groupId} disabled={mutation.isPending} onChange={(event) => setGroupId(event.target.value)}>{groups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</select></label>
        <label className="field field-full"><span>OPC UA Endpoint</span><input value={endpoint} disabled={mutation.isPending} placeholder="opc.tcp://10.10.1.21:4841/" onChange={(event) => setEndpoint(event.target.value)} /></label>
        <label className="field"><span>安全策略</span><select value={securityPolicy} disabled={mutation.isPending} onChange={(event) => { const value = event.target.value; setSecurityPolicy(value); setSecurityMode(value === 'None' ? 'None' : 'SignAndEncrypt'); }}><option value="None">None</option><option value="Basic256Sha256">Basic256Sha256</option></select></label>
        <label className="field"><span>消息安全模式</span><select value={securityMode} disabled={securityPolicy === 'None' || mutation.isPending} onChange={(event) => setSecurityMode(event.target.value)}><option value="None">None</option><option value="SignAndEncrypt">SignAndEncrypt</option></select></label>
        <label className="field"><span>用户名（可选）</span><input value={username} disabled={mutation.isPending} autoComplete="off" onChange={(event) => setUsername(event.target.value)} /></label>
        <label className="field"><span>{source ? '新密码（留空不修改）' : '密码（可选）'}</span><input type="password" value={password} disabled={clearPassword || mutation.isPending} autoComplete="new-password" onChange={(event) => setPassword(event.target.value)} /></label>
        {source?.hasPassword ? <label className="confirm-check field-full"><input type="checkbox" checked={clearPassword} disabled={mutation.isPending} onChange={(event) => { setClearPassword(event.target.checked); if (event.target.checked) setPassword(''); }} /><span className="checkbox-visual" /><span>清除已保存的下级密码</span></label> : null}
        <label className="confirm-check field-full"><input type="checkbox" checked={enabled} disabled={mutation.isPending} onChange={(event) => setEnabled(event.target.checked)} /><span className="checkbox-visual" /><span>启用该下级连接</span></label>
      </div>
      {!validEndpoint && endpoint !== 'opc.tcp://' ? <p className="field-error">请输入合法的 opc.tcp Endpoint。</p> : null}
      <InlineNotice><p>一个下级可以提供多个逻辑设备组。保存并完成发现后，在“串口 / 设备映射”中为各设备分组；这里的默认归属组接收未映射点、跨设备计算与共享命令节点，不会覆盖已有设备映射。</p></InlineNotice>
      {source ? <InlineNotice tone="warning"><p>修改默认归属或设备映射前，须结束在线 OPC Session 并等待相关命令完成；普通 Session 选源不需要断开连接。</p></InlineNotice> : null}
      <InlineNotice><p>凭据由后端使用 Windows DPAPI machine scope 加密，API 不会回传明文密码。</p></InlineNotice>
      {mutation.error ? <div className="form-error" role="alert">{apiErrorMessage(mutation.error)}</div> : null}
    </Modal>
  );
}

export function validOpcEndpoint(value: string): boolean {
  return /^opc\.tcp:\/\/[^\s/?#]+(?::\d+)?(?:\/[^\s?#]*)?$/i.test(value.trim());
}
