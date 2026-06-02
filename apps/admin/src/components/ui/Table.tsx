import type { ReactNode } from 'react';

export interface Column<Row> {
  key: string;
  header: ReactNode;
  /** Renders the cell for a row. */
  cell: (row: Row) => ReactNode;
  className?: string;
}

interface TableProps<Row> {
  columns: Column<Row>[];
  rows: Row[];
  rowKey: (row: Row) => string;
  empty?: ReactNode;
}

export function Table<Row>({ columns, rows, rowKey, empty }: TableProps<Row>) {
  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr className="border-b border-border bg-surface-2 text-left">
            {columns.map((c) => (
              <th
                key={c.key}
                className={`px-s-4 py-s-3 font-medium text-fg-muted ${c.className ?? ''}`}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="px-s-4 py-s-6 text-center text-fg-dim">
                {empty ?? 'Nothing here yet.'}
              </td>
            </tr>
          ) : (
            rows.map((row) => (
              <tr
                key={rowKey(row)}
                className="border-b border-border last:border-0 hover:bg-surface-2/60"
              >
                {columns.map((c) => (
                  <td key={c.key} className={`px-s-4 py-s-3 text-fg-2 ${c.className ?? ''}`}>
                    {c.cell(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
