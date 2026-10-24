import { ChevronLeft, ChevronRight } from 'lucide-react';
import { useEffect, useState } from 'react';

interface PaginationProps {
  page: number;
  pageSize: number;
  total: number;
  onPageChange: (page: number) => void;
  onPageSizeChange?: (size: number) => void;
}

export function readPageSize(params: URLSearchParams, fallback = 50): number {
  const size = Number(params.get('size'));
  return [25, 50, 100, 200].includes(size) ? size : fallback;
}

export function Pagination({ page, pageSize, total, onPageChange, onPageSizeChange }: PaginationProps) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const outOfRange = page > pages;
  const first = total === 0 || outOfRange ? 0 : (page - 1) * pageSize + 1;
  const last = outOfRange ? 0 : Math.min(total, page * pageSize);
  const [jump, setJump] = useState(String(page));
  useEffect(() => setJump(String(page)), [page]);
  return (
    <div className="pagination">
      <span>显示 {first} 到 {last}，共 {total} 条</span>
      <div className="pagination-buttons">
        {onPageSizeChange ? <label className="page-size">每页 <select aria-label="每页条数" value={pageSize} onChange={(e) => onPageSizeChange(Number(e.target.value))}>{[25, 50, 100, 200].map((size) => <option key={size} value={size}>{size} 条</option>)}</select></label> : null}
        <button type="button" className="icon-button pagination-button" aria-label="上一页" disabled={page <= 1} onClick={() => onPageChange(page - 1)}>
          <ChevronLeft aria-hidden="true" size={17} />
        </button>
        <span className="current-page">{page} / {pages} 页</span>
        <button type="button" className="icon-button pagination-button" aria-label="下一页" disabled={page >= pages} onClick={() => onPageChange(page + 1)}>
          <ChevronRight aria-hidden="true" size={17} />
        </button>
        {outOfRange ? <button className="button button-small button-secondary" type="button" onClick={() => onPageChange(pages)}>返回末页</button> : null}
        {pages > 1 ? <form className="pagination-jump" onSubmit={(event) => { event.preventDefault(); const next = Number(jump); if (Number.isInteger(next) && next >= 1 && next <= pages) onPageChange(next); }}>
          <input aria-label="跳转页码" type="number" min={1} max={pages} value={jump} onChange={(event) => setJump(event.target.value)} />
          <button className="button button-small button-secondary" type="submit">跳转</button>
        </form> : null}
      </div>
    </div>
  );
}
