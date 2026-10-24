import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { Group } from '../../api/types';
import { api, apiErrorMessage } from '../../api/client';
import { queryKeys } from '../../api/queries';
import { Modal } from '../../components/Modal';

interface Props { open: boolean; group?: Group; onClose: () => void; onSaved: (message: string) => void }

export function GroupFormModal(props: Props) {
  return props.open ? <GroupForm key={props.group?.id ?? 'new'} {...props} /> : null;
}

function GroupForm({ group, onClose, onSaved }: Props) {
  const client = useQueryClient();
  const mounted = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const [editGroup] = useState(group);
  const [id, setId] = useState(group?.id ?? '');
  const [name, setName] = useState(group?.name ?? '');
  const mutation = useMutation({
    mutationFn: () => editGroup
      ? api.updateGroup(editGroup.id, { id: editGroup.id, name: name.trim(), defaultSourceId: editGroup.defaultSourceId, revision: editGroup.selectionRevision })
      : api.createGroup({ id: id.trim(), name: name.trim() || id.trim() }),
    onSuccess: async (saved) => {
      await Promise.all([client.invalidateQueries({ queryKey: queryKeys.groups }), client.invalidateQueries({ queryKey: queryKeys.overview })]);
      if (!mounted.current) return;
      onSaved(`分组 ${saved.id} 已${editGroup ? '更新' : '创建'}`);
      onClose();
    },
  });

  const validId = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(id.trim());
  return (
    <Modal
      open
      title={group ? '编辑分组' : '新建分组'}
      description="Source ID 和 Group ID 是跨重启稳定的配置标识。"
      onClose={onClose}
      width="small"
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>{mutation.isPending ? '关闭（请求继续执行）' : '取消'}</button>
          <button className="button button-primary" type="button" disabled={!validId || mutation.isPending} onClick={() => mutation.mutate()}>{mutation.isPending ? '保存中…' : '保存'}</button>
        </>
      }
    >
      <div className="form-stack">
        <label className="field"><span>Group ID</span><input value={id} disabled={Boolean(editGroup) || mutation.isPending} maxLength={64} placeholder="例如 fire-panel-a" onChange={(event) => setId(event.target.value)} /></label>
        {!validId && id ? <p className="field-error">仅允许字母、数字、点、下划线和连字符，且必须以字母或数字开头。</p> : null}
        <label className="field"><span>显示名称</span><input value={name} disabled={mutation.isPending} maxLength={80} placeholder="默认与 Group ID 相同" onChange={(event) => setName(event.target.value)} /></label>
        {mutation.error ? <div className="form-error" role="alert">{apiErrorMessage(mutation.error)}</div> : null}
      </div>
    </Modal>
  );
}
