import { describe, expect, it } from 'vitest';
import { statusCodeHex, statusCodeLabel } from './labels';

describe('OPC UA status labels', () => {
  it('translates calculation communication quality', () => {
    expect(statusCodeLabel(statusCodeHex(0x80310000))).toContain('通信中断');
    expect(statusCodeLabel(statusCodeHex(0x80310000))).toContain('BadNoCommunication');
  });

  it('describes Good severity with the lower notification overflow bit', () => {
    const label = statusCodeLabel(statusCodeHex(0x00000080));
    expect(label).toContain('正常');
    expect(label).toContain('Good');
    expect(label).toContain('通知队列溢出');
  });
});
