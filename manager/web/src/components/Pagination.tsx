import { ChevronLeft, ChevronRight } from 'lucide-react';

interface PaginationProps {
  page: number;
  pageSize: number;
  total: number;
  onPageChange: (page: number) => void;
}

export function Pagination({ page, pageSize, total, onPageChange }: PaginationProps) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  const first = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const last = Math.min(total, page * pageSize);
  return (
    <div className="pagination">
      <span>显示 {first} 到 {last}，共 {total} 条</span>
      <div className="pagination-buttons">
        <button type="button" className="icon-button pagination-button" aria-label="上一页" disabled={page <= 1} onClick={() => onPageChange(page - 1)}>
          <ChevronLeft aria-hidden="true" size={17} />
        </button>
        <span className="current-page">{page}</span>
        <button type="button" className="icon-button pagination-button" aria-label="下一页" disabled={page >= pages} onClick={() => onPageChange(page + 1)}>
          <ChevronRight aria-hidden="true" size={17} />
        </button>
      </div>
    </div>
  );
}
