import { Modal } from '../../components/Modal';
import { InlineNotice } from '../../components/InlineNotice';

export function DeleteConfirmModal({ open, title, subject, pending, error, onClose, onConfirm }: { open: boolean; title: string; subject: string; pending: boolean; error?: string; onClose: () => void; onConfirm: () => void }) {
  return (
    <Modal
      open={open}
      title={title}
      description={`即将删除 ${subject}`}
      width="small"
      onClose={onClose}
      footer={
        <><button className="button button-secondary" type="button" onClick={onClose}>取消</button><button className="button button-danger" type="button" disabled={pending} onClick={onConfirm}>{pending ? '删除中…' : '确认删除'}</button></>
      }
    >
      <InlineNotice tone="warning"><p>若目标仍是默认源、被在线 Session 选中或存在待处理命令，服务会拒绝删除。</p></InlineNotice>
      {error ? <div className="form-error" role="alert">{error}</div> : null}
    </Modal>
  );
}
