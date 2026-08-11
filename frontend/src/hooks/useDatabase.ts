import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import {
  databaseApi,
  type ColumnDef,
  type DatabaseView,
  type Row,
  type ViewType,
} from "~/api/database";

// useDatabase bundles the three queries (database, rows, views) a
// DatabaseBlock needs alongside the mutations to update each. The
// callers stay declarative; this hook owns the invalidation.
//
// ⚠ IT TAKES THE VIEW *TYPE*, NOT A VIEW ID, AND RESOLVES THE ACTIVE VIEW ITSELF. It used to take
// `viewID?` — and the only caller could not supply one, because the id it needed came from the
// `views` query this same hook owns. So `listRows` was called with no view, the server's filters +
// sort were never asked for, and the component's own comment ("so the server can apply filters +
// sort") described a request that did not exist. Resolving the view here closes the circle, and
// returning `activeView` keeps ONE definition of "the view being rendered" instead of two.
// Pinned by DatabaseBlock.view.test.tsx.
export function useDatabase(dbID: string, viewType?: ViewType) {
  const qc = useQueryClient();

  const database = useQuery({
    queryKey: ["database", dbID],
    queryFn: () => databaseApi.get(dbID),
    enabled: !!dbID,
  });
  const views = useQuery({
    queryKey: ["database-views", dbID],
    queryFn: () => databaseApi.listViews(dbID),
    enabled: !!dbID,
  });
  const activeView = useMemo(
    () => (viewType ? (views.data ?? []).find((v) => v.type === viewType) : undefined),
    [views.data, viewType],
  );
  // `enabled` is deliberately NOT gated on a resolved view: a database whose views have not been
  // created yet must still list its rows (unfiltered), which is the first-load path every new
  // inline database takes. DatabaseBlock.view.test.tsx's [FIRST-LOAD] is the assertion on that.
  const rows = useQuery({
    queryKey: ["database-rows", dbID, activeView?.id ?? ""],
    queryFn: () => databaseApi.listRows(dbID, activeView?.id),
    enabled: !!dbID,
  });

  const invalidateAll = () => {
    qc.invalidateQueries({ queryKey: ["database", dbID] });
    qc.invalidateQueries({ queryKey: ["database-rows", dbID] });
    qc.invalidateQueries({ queryKey: ["database-views", dbID] });
  };

  const updateSchema = useMutation({
    mutationFn: (schema: ColumnDef[]) => databaseApi.updateSchema(dbID, schema),
    onSuccess: invalidateAll,
  });
  const createRow = useMutation({
    mutationFn: (body: Partial<Row>) => databaseApi.createRow(dbID, body),
    onSuccess: invalidateAll,
  });
  const updateRow = useMutation({
    mutationFn: ({ rowID, values }: { rowID: string; values: Record<string, unknown> }) =>
      databaseApi.updateRow(dbID, rowID, values),
    onSuccess: invalidateAll,
  });
  const deleteRow = useMutation({
    mutationFn: (rowID: string) => databaseApi.deleteRow(dbID, rowID),
    onSuccess: invalidateAll,
  });
  const createView = useMutation({
    mutationFn: (body: Partial<DatabaseView>) => databaseApi.createView(dbID, body),
    onSuccess: invalidateAll,
  });
  const updateView = useMutation({
    mutationFn: ({ viewID, updates }: { viewID: string; updates: Partial<DatabaseView> }) =>
      databaseApi.updateView(dbID, viewID, updates),
    onSuccess: invalidateAll,
  });

  return {
    database: database.data,
    views: views.data ?? [],
    activeView,
    rows: rows.data ?? [],
    isLoading: database.isLoading || rows.isLoading,
    updateSchema,
    createRow,
    updateRow,
    deleteRow,
    createView,
    updateView,
  };
}
