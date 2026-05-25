import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  databaseApi,
  type ColumnDef,
  type DatabaseView,
  type Row,
} from "~/api/database";

// useDatabase bundles the three queries (database, rows, views) a
// DatabaseBlock needs alongside the mutations to update each. The
// callers stay declarative; this hook owns the invalidation.
export function useDatabase(dbID: string, viewID?: string) {
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
  const rows = useQuery({
    queryKey: ["database-rows", dbID, viewID ?? ""],
    queryFn: () => databaseApi.listRows(dbID, viewID),
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
