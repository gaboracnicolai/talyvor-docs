import { Plus, Trash2 } from "lucide-react";
import { CellEditor } from "./CellEditor";
import type { ColumnDef, ColumnType, Row } from "~/api/database";

interface TableProps {
  schema: ColumnDef[];
  rows: Row[];
  onUpdateRow: (rowID: string, values: Record<string, unknown>) => void;
  onDeleteRow: (rowID: string) => void;
  onAddRow: () => void;
  onAddColumn: () => void;
  onRenameColumn: (colID: string, name: string) => void;
  onRetypeColumn: (colID: string, type: ColumnType) => void;
}

const COLUMN_TYPES: { value: ColumnType; label: string }[] = [
  { value: "text", label: "Text" },
  { value: "number", label: "Number" },
  { value: "select", label: "Select" },
  { value: "multi_select", label: "Multi-select" },
  { value: "date", label: "Date" },
  { value: "checkbox", label: "Checkbox" },
  { value: "url", label: "URL" },
  { value: "relation", label: "Relation" },
  { value: "formula", label: "Formula" },
];

// TableView is the spreadsheet-style projection. Cells delegate to
// CellEditor so the per-type behaviour stays in one place; the
// table only owns the row + column scaffolding.
export function TableView({
  schema,
  rows,
  onUpdateRow,
  onDeleteRow,
  onAddRow,
  onAddColumn,
  onRenameColumn,
  onRetypeColumn,
}: TableProps) {
  return (
    <div className="overflow-x-auto rounded border border-border">
      <table className="w-full text-xs">
        <thead className="bg-surface">
          <tr>
            {schema.map((col) => (
              <th
                key={col.id}
                className="border-b border-border px-2 py-1 text-left font-medium text-muted"
              >
                <div className="flex items-center gap-1">
                  <input
                    value={col.name}
                    onChange={(e) => onRenameColumn(col.id, e.target.value)}
                    className="flex-1 bg-transparent text-[10px] uppercase tracking-wider focus:outline-none"
                  />
                  <select
                    value={col.type}
                    onChange={(e) => onRetypeColumn(col.id, e.target.value as ColumnType)}
                    className="bg-transparent text-[9px] text-muted"
                    title="Change column type"
                  >
                    {COLUMN_TYPES.map((t) => (
                      <option key={t.value} value={t.value}>
                        {t.label}
                      </option>
                    ))}
                  </select>
                </div>
              </th>
            ))}
            <th className="w-8 border-b border-border bg-surface">
              <button
                onClick={onAddColumn}
                className="flex h-full w-full items-center justify-center text-muted hover:text-text"
                title="Add column"
              >
                <Plus size={11} />
              </button>
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id} className="border-t border-border">
              {schema.map((col) => (
                <td key={col.id} className="px-2 py-1 align-top">
                  <CellEditor
                    col={col}
                    value={r.values[col.id]}
                    onChange={(next) => onUpdateRow(r.id, { [col.id]: next })}
                  />
                </td>
              ))}
              <td className="w-8 align-top">
                <button
                  onClick={() => onDeleteRow(r.id)}
                  className="flex h-full w-full items-center justify-center text-muted hover:text-callout-error"
                  title="Delete row"
                >
                  <Trash2 size={10} />
                </button>
              </td>
            </tr>
          ))}
          <tr>
            <td colSpan={schema.length + 1}>
              <button
                onClick={onAddRow}
                className="flex w-full items-center gap-1 px-2 py-1 text-[10px] text-muted hover:text-text"
              >
                <Plus size={10} /> Add row
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  );
}
