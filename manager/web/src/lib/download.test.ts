import { describe, expect, it } from 'vitest';
import { csvCell } from './download';

describe('csv export cells', () => {
  it('quotes text and keeps spreadsheets from evaluating it as a formula', () => {
    expect(csvCell('rcd-46 "COM1"')).toBe('"rcd-46 ""COM1"""');
    expect(csvCell('=HYPERLINK("http://x","y")')).toBe(`"'=HYPERLINK(""http://x"",""y"")"`);
    for (const text of ['+1', '-1+2', '@SUM(A1)', '\tx', '\rx']) expect(csvCell(text).startsWith(`"'`)).toBe(true);
    expect(csvCell(undefined)).toBe('""');
    expect(csvCell(12)).toBe('"12"');
  });
});
