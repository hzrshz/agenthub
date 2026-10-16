import { ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight } from "lucide-react";

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
      <span className="info">
        {from}–{to} / {meta.total} RECORDS
      </span>
      {onPerPage && (
        <select value={meta.per_page} onChange={(e) => onPerPage(parseInt(e.target.value, 10))} aria-label="每页条数">
          {[20, 50, 100].map((n) => (
            <option key={n} value={n}>{n} / 页</option>
          ))}
        </select>
      )}
      <span className="pager">
        <button disabled={current <= 1} onClick={() => onPage(1)} aria-label="第一页"><ChevronsLeft size={13} /></button>
        <button disabled={current <= 1} onClick={() => onPage(current - 1)} aria-label="上一页"><ChevronLeft size={13} /></button>
        {pages.map((p, i) =>
          p === "…" ? (
            <span key={`e${i}`} className="ellipsis">…</span>
          ) : (
            <button key={p} className={p === current ? "active" : ""} onClick={() => onPage(p)}>
              {p}
            </button>
          ),
        )}
        <button disabled={current >= totalPages} onClick={() => onPage(current + 1)} aria-label="下一页"><ChevronRight size={13} /></button>
        <button disabled={current >= totalPages} onClick={() => onPage(totalPages)} aria-label="最后一页"><ChevronsRight size={13} /></button>
      </span>
      <span className="jump">
        PAGE
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
        / {totalPages}
      </span>
    </div>
  );
}

// FilterBar：调用记录页的筛选行。
// admin 视图：按用户下拉（选项由 stats API 提供）+ 结果下拉；
// 用户视图：仅结果下拉（自己的记录没有按用户/工具筛选的价值）。
import { RotateCcw, User as UserIcon } from "lucide-react";

export interface UserOption {
  user_id: string;
  username: string;
}

export function FilterBar({
  admin = false,
  userOptions = [],
  user,
  setUser,
  outcome,
  setOutcome,
  onReset,
  hasFilter,
}: {
  admin?: boolean;
  userOptions?: UserOption[];
  user?: string;
  setUser?: (v: string) => void;
  outcome: string;
  setOutcome: (v: string) => void;
  onReset: () => void;
  hasFilter: boolean;
}) {
  return (
    <div className="filter-row">
      {admin && (
        <select value={user ?? ""} onChange={(e) => setUser?.(e.target.value)} aria-label="按用户筛选">
          <option value="">全部用户</option>
          {userOptions.map((u) => (
            <option key={u.user_id} value={u.username}>{u.username}</option>
          ))}
        </select>
      )}
      <select value={outcome} onChange={(e) => setOutcome(e.target.value)} aria-label="按结果筛选">
        <option value="">全部结果</option>
        <option value="success">成功</option>
        <option value="error">失败</option>
        <option value="timeout">超时</option>
        <option value="cancelled">已取消</option>
      </select>
      {hasFilter && (
        <button type="button" onClick={onReset}><RotateCcw size={13} />重置</button>
      )}
      {admin && userOptions.length === 0 && (
        <span className="muted"><UserIcon size={12} style={{ verticalAlign: -2 }} /> 暂无用户数据</span>
      )}
    </div>
  );
}
