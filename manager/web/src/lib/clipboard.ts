// HTTP intranet deployments do not always expose the secure Clipboard API.
export async function copyText(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(value); return; } catch { /* Try same-click legacy copy. */ }
  }
  const previous = document.activeElement;
  const input = document.createElement('textarea');
  input.value = value; input.readOnly = true;
  input.style.position = 'fixed'; input.style.opacity = '0';
  document.body.appendChild(input);
  try {
    input.select();
    if (!document.execCommand?.('copy')) throw new Error('浏览器不允许自动复制，请手动复制');
  } finally {
    input.remove();
    if (previous instanceof HTMLElement) previous.focus();
  }
}
