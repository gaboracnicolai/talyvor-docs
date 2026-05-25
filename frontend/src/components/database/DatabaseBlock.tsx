import { useEffect, useMemo, useState } from "react";
import { LayoutGrid, List, Rows3, Table, Plus } from "lucide-react";
import { useDatabase } from "~/hooks/useDatabase";
import type { ColumnDef, ColumnType, ViewType } from "~/api/database";
import { TableView } from "./TableView";
import { ListView } from "./ListView";
import { KanbanView } from "./KanbanView";
import { GalleryView } from "./GalleryView";

interface BlockProps {
  databaseID: string;
}

const VIEW_TABS: { type: ViewType; label: string; icon: React.ReactNode }[] = [
  { type: "table", label: "Table", icon: <Table size={11} /> },
  { type: "list", label: "List", icon: <List size={11} /> },
  { type: "kanban", label: "Kanban", icon: <Rows3 size={11} /> },
  { type: "gallery", label: "Gallery", icon: <LayoutGrid size={11} /> },
];

// DatabaseBlock is the inline-database surface. It renders a tab
// strip for the view type, lets the user add rows from the active
// view, and delegates rendering to the per-view component. Schema
// + rows + views are all fetched from the backend; CRUD goes
// through the useDatabase hook.
export function DatabaseBlock({ databaseID }: BlockProps) {
  const [viewType, setViewType] = useState<ViewType>("table");
  const {
    database,
    views,
    rows,
    isLoading,
    updateSchema,
    createRow,
    updateRow,
    deleteRow,
    createView,
    updateView,
  } = useDatabase(databaseID);

  // Pick (or auto-create) a view that matches the active type so the
  // server can apply filters + sort. First load: ensure at least a
  // default table view exists.
  const activeView = useMemo(
    () => views.find((v) => v.type === viewType),
    [views, viewType],
  );
  useEffect(() => {
    if (!database || views.length > 0 || createView.isPending) return;
    createView.mutate({
      database_id: databaseID,
      name: "Table",
      type: "table",
      sort_dir: "asc",
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [database, views.length]);

  if (isLoading || !database) {
    return (
      <div className="my-2 rounded border border-border p-3 text-xs text-muted">
        Loading database…
      </div>
    );
  }

  const schema = database.schema ?? [];

  // Common mutation wrappers — the per-view components accept simple
  // callbacks rather than the mutation objects directly.
  const addRow = (preset: Record<string, unknown> = {}) =>
    createRow.mutate({ values: preset, position: rows.length + 1 });
  const patchRow = (rowID: string, values: Record<string, unknown>) =>
    updateRow.mutate({ rowID, values });
  const removeRow = (rowID: string) => deleteRow.mutate(rowID);

  const addColumn = () => {
    const next: ColumnDef = {
      id: "c-" + Math.random().toString(36).slice(2, 8),
      name: "New column",
      type: "text",
    };
    updateSchema.mutate([...schema, next]);
  };
  const renameColumn = (colID: string, name: string) => {
    updateSchema.mutate(schema.map((c) => (c.id === colID ? { ...c, name } : c)));
  };
  const retypeColumn = (colID: string, type: ColumnType) => {
    updateSchema.mutate(schema.map((c) => (c.id === colID ? { ...c, type } : c)));
  };

  // Find a select column to drive the kanban grouping. Users can
  // change this from the view's settings menu in a future polish
  // pass.
  const kanbanGroup =
    activeView?.group_by ||
    schema.find((c) => c.type === "select")?.id ||
    "";

  return (
    <div className="my-3 rounded-md border border-border bg-surface">
      <header className="flex items-center justify-between gap-2 border-b border-border px-2 py-1">
        <div className="flex items-center gap-1 text-xs font-semibold">
          {database.name}
        </div>
        <nav className="flex items-center gap-1">
          {VIEW_TABS.map((t) => (
            <button
              key={t.type}
              onClick={() => {
                setViewType(t.type);
                if (!views.some((v) => v.type === t.type)) {
                  createView.mutate({
                    database_id: databaseID,
                    name: t.label,
                    type: t.type,
                    sort_dir: "asc",
                  });
                }
                if (t.type === "kanban" && activeView && !activeView.group_by && kanbanGroup) {
                  updateView.mutate({
                    viewID: activeView.id,
                    updates: { group_by: kanbanGroup },
                  });
                }
              }}
              className={`flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] ${
                viewType === t.type
                  ? "bg-accent text-bg"
                  : "text-muted hover:text-text"
              }`}
            >
              {t.icon}
              {t.label}
            </button>
          ))}
          <button
            onClick={() => addRow()}
            className="ml-2 flex items-center gap-1 rounded bg-bg px-1.5 py-0.5 text-[10px] text-muted hover:text-text"
          >
            <Plus size={10} /> New
          </button>
        </nav>
      </header>

      <div className="p-2">
        {viewType === "table" ? (
          <TableView
            schema={schema}
            rows={rows}
            onUpdateRow={patchRow}
            onDeleteRow={removeRow}
            onAddRow={() => addRow()}
            onAddColumn={addColumn}
            onRenameColumn={renameColumn}
            onRetypeColumn={retypeColumn}
          />
        ) : viewType === "list" ? (
          <ListView schema={schema} rows={rows} />
        ) : viewType === "kanban" ? (
          <KanbanView
            schema={schema}
            rows={rows}
            groupBy={kanbanGroup}
            onUpdateRow={patchRow}
            onAddRow={(col) => addRow(kanbanGroup ? { [kanbanGroup]: col } : {})}
          />
        ) : (
          <GalleryView schema={schema} rows={rows} />
        )}
      </div>
    </div>
  );
}
