import { describe, expect, it } from 'vitest';
import { ApiError } from '../../api/client';
import { operationBusy, operationFailure } from './shared';

describe('lower operation failure guidance', () => {
  it.each(['busy', 'pending_command'])('explains 409 %s as pending work, not a revision conflict', (code) => {
    const error = new ApiError(409, { code, message: 'pending operation' }, 'rejected');
    expect(operationBusy(error)).toBe(true);
    const message = operationFailure(error);
    expect(message).toContain('待处理命令或运维操作');
    expect(message).toContain('不会自动重试');
    expect(message).toContain('请等待完成');
    expect(message).not.toContain('版本冲突');
  });

  it('still identifies a genuine revision conflict', () => {
    const error = new ApiError(409, { code: 'conflict', message: 'revision changed' }, 'rejected');
    expect(operationBusy(error)).toBe(false);
    expect(operationFailure(error)).toContain('版本冲突');
  });

  it('does not mistake an unavailable operation for a known pending rejection', () => {
    const error = new ApiError(504, { code: 'busy', message: 'response lost' }, 'timeout');
    expect(operationBusy(error)).toBe(false);
    expect(operationFailure(error)).toContain('操作结果未确认');
  });
});
