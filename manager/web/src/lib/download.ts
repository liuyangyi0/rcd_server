// Saves text as a file. The link is attached and the object URL revoked on the next task,
// because some browsers start the download only after click() returns.
export function downloadText(name: string, content: string, type = 'text/plain;charset=utf-8'): void {
  const url = URL.createObjectURL(new Blob([content], { type }));
  const link = document.createElement('a');
  link.href = url;
  link.download = name.replace(/[\\/:*?"<>|\u0000-\u001f]/g, '_');
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

// Quoted CSV cell. Text starting with = + - @, tab or carriage return gets a leading ' so a
// spreadsheet shows it instead of evaluating it: log and device text can come from the network.
export function csvCell(value: unknown): string {
  const text = String(value ?? '');
  return `"${(/^[=+\-@\t\r]/.test(text) ? `'${text}` : text).replaceAll('"', '""')}"`;
}

export function downloadCsv(name: string, rows: unknown[][]): void {
  downloadText(name, `﻿${rows.map((row) => row.map(csvCell).join(',')).join('\r\n')}`, 'text/csv;charset=utf-8');
}
