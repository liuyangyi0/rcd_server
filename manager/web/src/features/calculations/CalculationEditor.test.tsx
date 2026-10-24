import { createRef } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import CalculationEditor, { type CalculationEditorHandle } from './CalculationEditor';

describe('CalculationEditor', () => {
  it('renders large TOML in bounded rule chunks and rejoins edits without loss', async () => {
    const rules = Array.from({ length: 205 }, (_, index) => `[[rules]]\nkks_calc = "K${index + 1}"\nexpression = "${index}"\n`);
    const content = `schema = 1\n${rules.join('')}`;
    const ref = createRef<CalculationEditorHandle>();
    const dirty = vi.fn();
    render(<CalculationEditor ref={ref} defaultValue={content} onDirty={dirty} />);

    const editor = screen.getByRole('textbox');
    expect((editor as HTMLTextAreaElement).value).toContain('K100');
    expect((editor as HTMLTextAreaElement).value).not.toContain('K101');
    fireEvent.change(editor, { target: { value: `${String((editor as HTMLTextAreaElement).value)}# page edit\n` } });
    expect(dirty).toHaveBeenCalledOnce();
    expect(ref.current?.getValue()).toContain('# page edit\n[[rules]]\nkks_calc = "K101"');

    ref.current?.focusProblem({ ruleIndex: 205, message: 'test' });
    await waitFor(() => expect((screen.getByRole('textbox') as HTMLTextAreaElement).value).toContain('K205'));
    expect(ref.current?.getValue().startsWith('schema = 1\n')).toBe(true);
    expect(ref.current?.getValue()).toContain('kks_calc = "K205"');
  });
});
