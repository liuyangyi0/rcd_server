import { forwardRef, useImperativeHandle, useRef } from 'react';
import type { CalculationProblem } from '../../api/types';

export interface CalculationEditorHandle {
	getValue(): string;
  focusProblem(problem: CalculationProblem): void;
}

interface Props {
	defaultValue: string;
  disabled?: boolean;
	onDirty(): void;
}

const RULE_MARKER = '[[rules]]';

const CalculationEditor = forwardRef<CalculationEditorHandle, Props>(function CalculationEditor({ defaultValue, disabled, onDirty }, ref) {
  const editor = useRef<HTMLTextAreaElement>(null);
  useImperativeHandle(ref, () => ({
	getValue: () => editor.current?.value ?? defaultValue,
    focusProblem(problem) {
      const target = editor.current;
      if (!target) return;
		const value = target.value;
      let offset = 0;
      if (problem.ruleIndex && problem.ruleIndex > 0) {
        let cursor = 0;
        for (let index = 0; index < problem.ruleIndex; index += 1) {
          const found = value.indexOf(RULE_MARKER, cursor);
          if (found < 0) break;
          offset = found;
          cursor = found + RULE_MARKER.length;
        }
      }
      target.focus();
      target.setSelectionRange(offset, Math.min(value.length, offset + RULE_MARKER.length));
      const lines = value.slice(0, offset).split('\n').length;
      target.scrollTop = Math.max(0, (lines - 5) * 20);
    },
	}), [defaultValue]);

  return <textarea
    ref={editor}
    className="calculation-editor"
    aria-label="中央计算 TOML 编辑器"
    spellCheck={false}
    disabled={disabled}
	defaultValue={defaultValue}
	onChange={onDirty}
  />;
});

export default CalculationEditor;
