import { forwardRef, useEffect, useImperativeHandle, useRef, useState, type MutableRefObject } from 'react';
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

interface EditorChunk {
  content: string;
  firstRule: number;
  lastRule: number;
}

const RULE_MARKER = '[[rules]]';
const RULES_PER_PAGE = 100;

const CalculationEditor = forwardRef<CalculationEditorHandle, Props>(function CalculationEditor({ defaultValue, disabled, onDirty }, ref) {
  const editor = useRef<HTMLTextAreaElement>(null);
  const chunksRef = useRef(splitIntoChunks(defaultValue));
  const pendingRule = useRef<number | undefined>(undefined);
  const [chunks, setChunks] = useState(chunksRef.current);
  const [page, setPage] = useState(0);
  const current = chunks[Math.min(page, chunks.length - 1)];
  const ruleCount = chunks.reduce((count, chunk) => count + Math.max(0, chunk.lastRule - chunk.firstRule + 1), 0);

  useImperativeHandle(ref, () => ({
    getValue: () => chunksRef.current.map((chunk) => chunk.content).join(''),
    focusProblem(problem) {
      const rule = problem.ruleIndex && problem.ruleIndex > 0 ? problem.ruleIndex : 1;
      const nextPage = Math.min(chunksRef.current.length - 1, Math.max(0, Math.floor((rule - 1) / RULES_PER_PAGE)));
      pendingRule.current = problem.ruleIndex;
      setPage(nextPage);
      if (nextPage === page) focusPendingRule(editor.current, pendingRule);
    },
  }), [page]);

  useEffect(() => {
    if (pendingRule.current === undefined) return;
    focusPendingRule(editor.current, pendingRule);
  }, [page]);

  const updateCurrent = (value: string) => {
    const next = chunksRef.current.map((chunk, index) => index === page ? { ...chunk, content: value } : chunk);
    chunksRef.current = next;
    setChunks(next);
    onDirty();
  };

  return <div className="calculation-editor-shell">
    <div className="calculation-editor-pagination">
      <div><strong>分段编辑</strong><span>仅渲染当前 100 条规则，保存时自动合并完整 TOML</span></div>
      <div className="calculation-editor-page-controls">
        <button className="button button-small button-secondary" type="button" disabled={page === 0} onClick={() => setPage((value) => Math.max(0, value - 1))}>上一段</button>
        <label><span className="visually-hidden">规则分段</span><select value={page} onChange={(event) => setPage(Number(event.target.value))}>{chunks.map((chunk, index) => <option key={`${chunk.firstRule}:${index}`} value={index}>{chunk.firstRule ? `规则 ${chunk.firstRule}–${chunk.lastRule}` : '配置头'}</option>)}</select></label>
        <button className="button button-small button-secondary" type="button" disabled={page >= chunks.length - 1} onClick={() => setPage((value) => Math.min(chunks.length - 1, value + 1))}>下一段</button>
      </div>
      <small>共 {ruleCount.toLocaleString('zh-CN')} 条规则 · 第 {page + 1}/{chunks.length} 段</small>
    </div>
    <textarea
      ref={editor}
      className="calculation-editor"
      aria-label={`中央计算 TOML 编辑器，${current.firstRule ? `规则 ${current.firstRule} 至 ${current.lastRule}` : '配置头'}`}
      spellCheck={false}
      disabled={disabled}
      value={current.content}
      onChange={(event) => updateCurrent(event.target.value)}
    />
  </div>;
});

function splitIntoChunks(value: string): EditorChunk[] {
  const starts: number[] = [];
  let cursor = 0;
  while (cursor < value.length) {
    const found = value.indexOf(RULE_MARKER, cursor);
    if (found < 0) break;
    starts.push(found);
    cursor = found + RULE_MARKER.length;
  }
  if (!starts.length) return [{ content: value, firstRule: 0, lastRule: 0 }];
  const prefix = value.slice(0, starts[0]);
  const rules = starts.map((start, index) => value.slice(start, starts[index + 1] ?? value.length));
  const chunks: EditorChunk[] = [];
  for (let offset = 0; offset < rules.length; offset += RULES_PER_PAGE) {
    const pageRules = rules.slice(offset, offset + RULES_PER_PAGE);
    chunks.push({ content: `${offset === 0 ? prefix : ''}${pageRules.join('')}`, firstRule: offset + 1, lastRule: offset + pageRules.length });
  }
  return chunks;
}

function focusPendingRule(target: HTMLTextAreaElement | null, pending: MutableRefObject<number | undefined>) {
  if (!target) return;
  const rule = pending.current;
  pending.current = undefined;
  let offset = 0;
  if (rule && rule > 0) {
    const localIndex = (rule - 1) % RULES_PER_PAGE;
    let cursor = 0;
    for (let index = 0; index <= localIndex; index += 1) {
      const found = target.value.indexOf(RULE_MARKER, cursor);
      if (found < 0) break;
      offset = found;
      cursor = found + RULE_MARKER.length;
    }
  }
  window.requestAnimationFrame(() => {
    target.focus();
    target.setSelectionRange(offset, Math.min(target.value.length, offset + RULE_MARKER.length));
    const lines = target.value.slice(0, offset).split('\n').length;
    target.scrollTop = Math.max(0, (lines - 5) * 20);
  });
}

export default CalculationEditor;
