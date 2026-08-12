import { useEffect, useState } from "react";
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
  // The view type goes INTO the hook: it resolves the matching saved view and puts its id on the
  // rows request, which is what makes the server apply that view's filters + sort. Resolving it
  // here instead left the rows query with no view id at all — see useDatabase's header and
  // DatabaseBlock.view.test.tsx.
  const {
    database,
    views,
    activeView,
    rows,
    isLoading,
    updateSchema,
    createRow,
    updateRow,
    deleteRow,
    createView,
    updateView,
  } = useDatabase(databaseID, viewType);

  // First load: ensure at least a default table view exists.
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

  // `schema` is the DATABASE's whole column set and is what every WRITE path sends back.
  // `visibleSchema` is what this VIEW renders. They are two different things and conflating them
  // is destructive — see below.
  const schema = database.schema ?? [];

  // The active view's hidden columns, applied. `hidden_cols` is stored, served on the views
  // payload and declared in api/database.ts, and until this line it appeared in no component:
  // every renderer was handed the whole schema. Measured through the shipped routes on real
  // Postgres — the server stores and returns hidden_cols, and `GET /rows` returns every cell
  // regardless, because it returns ROWS and not columns. Hiding is therefore the renderer's job
  // and there is nowhere else it can happen. DatabaseBlock.hiddencols.test.tsx holds it.
  //
  // ⚠ THE FILTER MUST NOT MOVE UP INTO `schema`. `addColumn` sends `[...schema, next]` to
  // `PATCH /databases/{dbID}/schema`, which replaces the WHOLE schema — so a filtered `schema`
  // would DELETE every hidden column, and its cells, the next time anyone added one.
  // [SCHEMA-INTACT] is the assertion on that, and it is green before this change as well as
  // after: it exists to stay green, not to turn.
  const hiddenCols = new Set(activeView?.hidden_cols ?? []);
  const visibleSchema = schema.filter((c) => !hiddenCols.has(c.id));

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
  //
  // Resolved over the VISIBLE schema, so one rule covers the whole component: a column this view
  // hides is not part of the view's column set, and you cannot group by a column you have hidden.
  // With `hidden_cols` empty — every view this SPA creates — `visibleSchema` IS `schema`, so this
  // is byte-for-byte the previous behaviour.
  const kanbanGroup =
    activeView?.group_by ||
    visibleSchema.find((c) => c.type === "select")?.id ||
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
            schema={visibleSchema}
            rows={rows}
            onUpdateRow={patchRow}
            onDeleteRow={removeRow}
            onAddRow={() => addRow()}
            onAddColumn={addColumn}
            onRenameColumn={renameColumn}
            onRetypeColumn={retypeColumn}
          />
        ) : viewType === "list" ? (
          <ListView schema={visibleSchema} rows={rows} />
        ) : viewType === "kanban" ? (
          <KanbanView
            schema={visibleSchema}
            rows={rows}
            groupBy={kanbanGroup}
            onUpdateRow={patchRow}
            onAddRow={(col) => addRow(kanbanGroup ? { [kanbanGroup]: col } : {})}
          />
        ) : (
          <GalleryView schema={visibleSchema} rows={rows} />
        )}
      </div>
    </div>
  );
}
