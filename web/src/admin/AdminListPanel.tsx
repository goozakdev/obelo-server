import type { ReactNode } from "react";

// The shape every list in the Admin section takes: a filled header bar carrying a
// title and/or a count (plus an optional primary action pinned right) over a
// framed body holding the list — or, in the same frame, whatever loading / error /
// empty line stands in for it. The panel is what makes the Libraries, Attention,
// Devices, and Users tabs read as one system instead of four ad-hoc stacks.
//
// The panel owns the chrome only; each caller keeps its own list markup, row
// classes, and states inside `children`. Rows opt into the shared band geometry
// (padding + the hairline between rows) with `.admin-panel-row`.
//
// `title` renders as an <h3>, so a tab with several panels (Attention) is
// navigable by heading; a tab whose single panel is already named by the nav rail
// (Users, Libraries, Devices) passes only a `count` and gets no redundant heading.

export default function AdminListPanel({
  title,
  count,
  countTestId,
  action,
  testId,
  children,
}: {
  /** Section heading in the bar — omit when the nav rail already names the list. */
  title?: string;
  /** Dim text beside the title, e.g. "3 users" or a bare item count. */
  count?: string;
  countTestId?: string;
  /** Primary action pinned to the right of the bar (e.g. an Add button). */
  action?: ReactNode;
  testId?: string;
  children: ReactNode;
}) {
  return (
    <div className="admin-panel" data-testid={testId}>
      <div className="admin-panel-bar">
        <span className="admin-panel-label">
          {title && <h3 className="admin-panel-title">{title}</h3>}
          {count !== undefined && (
            <span className="admin-panel-count" data-testid={countTestId}>
              {count}
            </span>
          )}
        </span>
        {action}
      </div>
      <div className="admin-panel-body">{children}</div>
    </div>
  );
}
