import { useEffect, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import type { Group } from '../../api/types';
import { api, apiErrorMessage } from '../../api/client';
import { queryKeys } from '../../api/queries';
import { Modal } from '../../components/Modal';

export function GroupFormModal({ open, group, onClose, onSaved }: { open: boolean; group?: Group; onClose: () => void; onSaved: (message: string) => void }) {
  const client = useQueryClient();
  const [id, setId] = useState('');
  const [name, setName] = useState('');
  const mutation = useMutation({
    mutationFn: () => group
      ? api.updateGroup(group.id, { id: group.id, name: name.trim(), defaultSourceId: group.defaultSourceId, revision: group.selectionRevision })
      : api.createGroup({ id: id.trim(), name: name.trim() || id.trim() }),
    onSuccess: async (saved) => {
      await Promise.all([client.invalidateQueries({ queryKey: queryKeys.groups }), client.invalidateQueries({ queryKey: queryKeys.overview })]);
      onSaved(`分组 ${saved.id} 已${group ? '更新' : '创建'}`);
      onClose();
    },
  });

  useEffect(() => {
    if (!open) return;
    setId(group?.id ?? '');
    setName(group?.name ?? '');
    mutation.reset();
  }, [group, open]); // mutation instance is stable

  const validId = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(id.trim());
  return (
    <Modal
      open={open}
      title={group ? '编辑分组' : '新建分组'}
      description="Source ID 和 Group ID 是跨重启稳定的配置标识。"
      onClose={onClose}
      width="small"
      footer={
        <>
          <button className="button button-secondary" type="button" onClick={onClose}>取消</button>
          <button className="button button-primary" type="button" disabled={!validId || mutation.isPending} onClick={() => mutation.mutate()}>{mutation.isPending ? '保存中…' : '保存'}</button>
        </>
      }
    >
      <div className="form-stack">
        <label className="field"><span>Group ID</span><input value={id} disabled={Boolean(group)} maxLength={64} placeholder="例如 fire-panel-a" onChange={(event) => setId(event.target.value)} /></label>
        {!validId && id ? <p className="field-error">仅允许字母、数字、点、下划线和连字符，且必须以字母或数字开头。</p> : null}
        <label className="field"><span>显示名称</span><input value={name} maxLength={80} placeholder="默认与 Group ID 相同" onChange={(event) => setName(event.target.value)} /></label>
        {mutation.error ? <div className="form-error" role="alert">{apiErrorMessage(mutation.error)}</div> : null}
      </div>
    </Modal>
  );
}
