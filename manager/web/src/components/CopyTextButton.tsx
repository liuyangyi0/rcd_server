import { useState } from 'react';
import { Check, Copy } from 'lucide-react';

export function CopyTextButton({ value, label = '复制' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="copy-text-button"
      title={`${label}：${value}`}
      aria-label={label}
      onClick={() => {
        void navigator.clipboard.writeText(value).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1200);
        });
      }}
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
      <span>{copied ? '已复制' : label}</span>
    </button>
  );
}
