import { useEffect, useRef, useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { copyText } from '../lib/clipboard';

export function CopyTextButton({ value, label = '复制' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  return (
    <span className="copy-control">
    <button
      type="button"
      className="copy-text-button"
      title={`${label}：${value}`}
      aria-label={label}
      onClick={() => {
        setFailed(false);
        void copyText(value).then(() => {
          setCopied(true);
          clearTimeout(timer.current);
          timer.current = setTimeout(() => setCopied(false), 1600);
        }).catch(() => { setCopied(false); setFailed(true); });
      }}
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      <span>{copied ? '已复制' : label}</span>
    </button>
    {failed ? <span className="copy-fallback" role="status">请手动复制：<input aria-label={`手动复制：${label}`} readOnly value={value} onFocus={(event) => event.currentTarget.select()} /></span> : null}
    </span>
  );
}
