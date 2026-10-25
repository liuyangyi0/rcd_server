import { ApiError, apiErrorMessage } from '../../api/client';

export function operationFailure(error: unknown): string {
  if (operationBusy(error)) return `下级仍有待处理命令或运维操作；不会自动重试。已请求刷新实际状态，请等待完成后再操作。${apiErrorMessage(error)}`;
  if (error instanceof ApiError && error.status === 409) return `版本冲突，已请求刷新实际状态；请核对后再操作。${apiErrorMessage(error)}`;
  if (!(error instanceof ApiError) || error.status === 0 || error.status >= 500) return `操作结果未确认，不会自动重试；已请求刷新，请核对下级实际状态。${apiErrorMessage(error)}`;
  return `操作未完成：${apiErrorMessage(error)}`;
}

export function operationBusy(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && (error.code === 'busy' || error.code === 'pending_command');
}

export function byteSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}

export { downloadText } from '../../lib/download';
