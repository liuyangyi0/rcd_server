import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useRef } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { operationsApi, type ConfigForm } from '../../api/operations';
import { ConfigParameters, type ConfigParametersHandle } from './ConfigParameters';

const target = { id: 'a', revision: 5 };
const content = '# comment\n[settings]\ncommand_retry_count=3\n';
const form: ConfigForm = { source_id: 'a', source_revision: 5, content, fields: [{ path: ['settings', 'command_retry_count'], key: 'settings.command_retry_count', label: '命令最大下发次数（含首次）', hint: '收到 ACK 后立即停止', kind: 'integer', value: 3, options: [], inherited: false, readonly: false }] };
afterEach(() => vi.restoreAllMocks());

function Harness({ onApplied = vi.fn(), onDirtyChange = vi.fn(), maxBytes }: { onApplied?: (content: string) => void; onDirtyChange?: (dirty: boolean) => void; maxBytes?: number }) {
  const ref = useRef<ConfigParametersHandle>(null);
  return <><ConfigParameters preparationRef={ref} maxBytes={maxBytes} target={target} content={content} disabled={false} onApplied={onApplied} onDirtyChange={onDirtyChange} onBusyChange={vi.fn()} /><button onClick={() => { void ref.current?.prepare().catch(() => undefined); }}>校验参数</button></>;
}

describe('configuration parameters', () => {
  it('prepares an explicit typed draft and never saves or auto-restarts', async () => {
    const prepare = vi.spyOn(operationsApi, 'prepare').mockResolvedValue(form);
    const save = vi.spyOn(operationsApi, 'save');
    const onApplied = vi.fn(); const onDirtyChange = vi.fn();
    render(<Harness onApplied={onApplied} onDirtyChange={onDirtyChange} />);
    const input = await screen.findByLabelText('命令最大下发次数（含首次）');
    fireEvent.change(input, { target: { value: '5' } });
    expect(onApplied).not.toHaveBeenCalled();
    expect(onDirtyChange).toHaveBeenCalledWith(true);
    prepare.mockResolvedValue({ ...form, content: content.replace('=3', '=5') });
    await userEvent.click(screen.getByRole('button', { name: '校验参数' }));
    await waitFor(() => expect(onApplied).toHaveBeenCalledWith(content.replace('=3', '=5')));
    expect(prepare.mock.calls.at(-1)?.[3]).toEqual([{ path: ['settings', 'command_retry_count'], value: 5 }]);
    expect(save).not.toHaveBeenCalled();
  });
  it('retains parameters after a failed preparation and blocks fractional counts', async () => {
    const prepare = vi.spyOn(operationsApi, 'prepare').mockResolvedValue(form);
    render(<Harness />);
    const input = await screen.findByLabelText('命令最大下发次数（含首次）');
    fireEvent.change(input, { target: { value: '2.5' } });
    await userEvent.click(screen.getByRole('button', { name: '校验参数' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('整数');
    expect(prepare).toHaveBeenCalledTimes(1);
    fireEvent.change(input, { target: { value: '5' } });
    prepare.mockRejectedValue(new Error('connection unavailable'));
    await userEvent.click(screen.getByRole('button', { name: '校验参数' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('connection unavailable'));
    expect(input).toHaveValue(5);
  });
  it('retains edits when the merged file exceeds the lower size limit', async () => {
    const prepare = vi.spyOn(operationsApi, 'prepare').mockResolvedValue(form);
    const onApplied = vi.fn();
    render(<Harness onApplied={onApplied} maxBytes={100} />);
    const input = await screen.findByLabelText('命令最大下发次数（含首次）');
    fireEvent.change(input, { target: { value: '5' } });
    prepare.mockResolvedValue({ ...form, content: 'x'.repeat(101) });
    await userEvent.click(screen.getByRole('button', { name: '校验参数' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('超过允许大小');
    expect(input).toHaveValue(5); expect(onApplied).not.toHaveBeenCalled();
  });
  it('defaults to common parameters but searches and selects advanced logging settings', async () => {
    vi.spyOn(operationsApi, 'prepare').mockResolvedValue({ ...form, fields: [...form.fields, { ...form.fields[0], key: 'logging.http_push.queue_capacity', path: ['logging', 'http_push', 'queue_capacity'], label: 'logging.http_push.queue_capacity', value: 10000 }] });
    render(<Harness />);
    await screen.findByLabelText('命令最大下发次数（含首次）');
    expect(screen.queryByLabelText('日志内存队列容量（条）')).not.toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText('参数显示范围'), '日志');
    expect(screen.getByLabelText('日志内存队列容量（条）')).toHaveValue(10000);
    expect(screen.queryByLabelText('命令最大下发次数（含首次）')).not.toBeInTheDocument();
    await userEvent.type(screen.getByRole('searchbox'), '下发');
    expect(screen.getByLabelText('命令最大下发次数（含首次）')).toBeInTheDocument();
    expect(screen.getByText(/搜索范围为全部参数/)).toBeInTheDocument();
  });
});
