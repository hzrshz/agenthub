// 通用分页条：页码、跳页、每页条数。
// 服务端分页契约：{ items, page, per_page, total }（与后端 audit.InvokePage 对齐）。
export interface PageMeta {
  page: number;
  per_page: number;
  total: number;
}

export function Pagination({
  meta,
  onPage,
  onPerPage,
}: {
  meta: PageMeta;
  onPage: (page: number) => void;
  onPerPage?: (perPage: number) => void;
}) {
  const totalPages = Math.max(1, Math.ceil(meta.total / meta.per_page));
  const current = Math.min(meta.page, totalPages);
  const from = meta.total === 0 ? 0 : (current - 1) * meta.per_page + 1;
  const to = Math.min(current * meta.per_page, meta.total);

  // 页码窗口：当前页前后各 2 页 + 首尾，超出用省略号。
  const pages: (number | "…")[] = [];
  const push = (v: number | "…") => {
    if (pages[pages.length - 1] !== v) pages.push(v);
  };
  const windowStart = Math.max(2, current - 2);
  const windowEnd = Math.min(totalPages - 1, current + 2);
  pages.push(1);
  if (windowStart > 2) push("…");
  for (let p = windowStart; p <= windowEnd; p++) push(p);
  if (windowEnd < totalPages - 1) push("…");
  if (totalPages > 1) push(totalPages);

  return (
    <div className="pagination">
      <span className="muted">
        第 {from}-{to} 条 / 共 {meta.total} 条
      </span>
      <span className="pager">
        <button disabled={current <= 1} onClick={() => onPage(1)}>«</button>
        <button disabled={current <= 1} onClick={() => onPage(current - 1)}>‹</button>
        {pages.map((p, i) =>
          p === "…" ? (
            <span key={`e${i}`} className="ellipsis">…</span>
          ) : (
            <button key={p} className={p === current ? "active" : ""} onClick={() => onPage(p)}>
              {p}
            </button>
          ),
        )}
        <button disabled={current >= totalPages} onClick={() => onPage(current + 1)}>›</button>
        <button disabled={current >= totalPages} onClick={() => onPage(totalPages)}>»</button>
      </span>
      <span className="jump">
        跳至
        <input
          type="number"
          min={1}
          max={totalPages}
          defaultValue={current}
          key={current}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              const v = parseInt((e.target as HTMLInputElement).value, 10);
              if (v >= 1 && v <= totalPages) onPage(v);
            }
          }}
          onBlur={(e) => {
            const v = parseInt(e.target.value, 10);
            if (v >= 1 && v <= totalPages && v !== current) onPage(v);
          }}
        />
        页
      </span>
      {onPerPage && (
        <select value={meta.per_page} onChange={(e) => onPerPage(parseInt(e.target.value, 10))}>
          {[20, 50, 100].map((n) => (
            <option key={n} value={n}>{n} 条/页</option>
          ))}
        </select>
      )}
    </div>
  );
}
