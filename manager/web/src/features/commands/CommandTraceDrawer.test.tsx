import { render, screen } from '@testing-library/react';
import { expect, it } from 'vitest';
import { CommandTraceBody, type CommandTraceDetail } from './CommandTraceDrawer';
import golden from '../../../../../contracts/command-tracking/v1/ack-received.json';

it('describes a queued command as not yet sent rather than a missing audit', () => {
  render(<CommandTraceBody detail={{ command: { id: 'q', source_id: 's', port: 'p', state: 'Queued' } }} />);
  expect(screen.getByText('排队待发送')).toBeInTheDocument();
  expect(screen.getByText(/尚未发送给下级/)).toBeInTheDocument();
  expect(screen.queryByText(/旧协议或历史记录/)).not.toBeInTheDocument();
});

it('shows late ACK separately from the original timeout and keeps incomplete evidence explicit', () => {
  const detail: CommandTraceDetail = { command: { id: 'c', source_id: 's', port: 'p', state: 'TimedOut' },
    trace: { protocol: 'tracked', gateway_instance_id: 'g', boot_id: 'b', history_gap: false,
      snapshot: { phase: 'AckReceived', accepted: true, ack_id: '18446744073709551615', attempt_count: 3, sent_count: 3,
        audit_incomplete: true, error: '', updated_at: '2026-09-22T00:00:00Z', attempts: [
          { attempt: 1, sent: true, result: 'ack_timeout', at: '2026-09-22T00:00:00Z' },
          { attempt: 3, sent: true, result: 'AckReceived', at: '2026-09-22T00:00:00Z' }] } } };
  render(<CommandTraceBody detail={detail} />);
  expect(screen.getByText(/原超时结果保留/)).toBeInTheDocument();
  expect(screen.getByRole('alert')).toHaveTextContent('不完整');
  expect(screen.getByText(/补发 2 次/)).toBeInTheDocument();
  expect(screen.getByText('18446744073709551615')).toBeInTheDocument();
});

it('does not invent counts for legacy or missing evidence', () => {
  render(<CommandTraceBody detail={{ command: { id: 'old', source_id: 's', port: 'p', state: 'Received' } }} />);
  expect(screen.getByText('旧协议或历史记录，无法精确关联')).toBeInTheDocument();
  expect(screen.queryByText(/完整写出 0 次/)).not.toBeInTheDocument();
});

it('renders the Rust and Go shared wire contract without rounding the lower ID', () => {
  render(<CommandTraceBody detail={{command:{id:golden.command_id,source_id:'s',port:golden.port_id,state:'Received'},trace:{protocol:'tracked',gateway_instance_id:golden.gateway_instance_id,boot_id:golden.boot_id,history_gap:false,snapshot:golden}}} />);
  expect(screen.getByText('18446744073709551615')).toBeInTheDocument();
  expect(screen.getByText(/调用发送 2 次/)).toBeInTheDocument();
});

it('distinguishes a recovered lower snapshot from missing intermediate sync events', () => {
  render(<CommandTraceBody detail={{command:{id:golden.command_id,source_id:'s',port:golden.port_id,state:'Received'},trace:{protocol:'tracked',gateway_instance_id:golden.gateway_instance_id,boot_id:golden.boot_id,history_gap:true,snapshot:{...golden,audit_incomplete:false}}}} />);
  expect(screen.getByRole('status')).toHaveTextContent('已取得下级执行快照');
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  expect(screen.getByText(/调用发送 2 次/)).toBeInTheDocument();
});
