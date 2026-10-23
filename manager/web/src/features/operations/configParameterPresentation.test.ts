import { describe, expect, it } from 'vitest';
import type { ConfigField } from '../../api/operations';
import { parameterOption, parameterPresentation } from './configParameterPresentation';

function field(key: string): ConfigField { return { key, path: key.split('.'), label: key, hint: '', value: 0, kind: 'integer', options: [], inherited: false, readonly: false }; }
describe('parameter presentation', () => {
  it('keeps lower metadata but supplies Chinese labels for known technical fallbacks', () => {
    const log = field('logging.http_push.spool_max_bytes');
    expect(parameterPresentation(log)).toMatchObject({ label: '日志补传缓存上限（字节）', group: '日志', common: false });
    expect(parameterPresentation({ ...log, label: '下级定义名称', hint: '下级定义说明' })).toMatchObject({ label: '下级定义名称', hint: '下级定义说明' });
    expect(parameterPresentation(field('custom.unknown')).label).toBe('custom.unknown');
  });
  it('groups command, polling and communication settings without changing paths or values', () => {
    expect(parameterPresentation(field('settings.command_interval_ms'))).toMatchObject({ common: true, group: '控制命令' });
    expect(parameterPresentation(field('settings.timeout'))).toMatchObject({ common: true, group: '采集轮询' });
    expect(parameterPresentation(field('ports.0.receive_timeout_ms'))).toMatchObject({ common: true, group: '串口与通信' });
    expect(parameterOption(field('calculation.mode'), 'manager')).toBe('管理端计算（manager）');
    expect(parameterOption(field('x'), false)).toBe('停用');
    expect(parameterOption(field('x'), 500)).toBe('500');
  });
});
