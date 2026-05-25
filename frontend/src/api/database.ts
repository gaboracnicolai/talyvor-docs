import { apiRequest } from "./client";

export type ColumnType =
  | "text"
  | "number"
  | "select"
  | "multi_select"
  | "date"
  | "checkbox"
  | "url"
  | "relation"
  | "formula";

export interface ColumnDef {
  id: string;
  name: string;
  type: ColumnType;
  options?: string[];
  formula?: string;
}

export interface Database {
  id: string;
  page_id: string;
  workspace_id: string;
  name: string;
  schema: ColumnDef[];
  created_at: string;
  updated_at: string;
}

export interface Row {
  id: string;
  database_id: string;
  values: Record<string, unknown>;
  position: number;
  created_at: string;
  updated_at: string;
}

export type ViewType = "table" | "list" | "kanban" | "gallery";

export interface Filter {
  col_id: string;
  operator: "eq" | "neq" | "contains" | "gt" | "lt";
  value: string;
}

export interface DatabaseView {
  id: string;
  database_id: string;
  name: string;
  type: ViewType;
  filters: Filter[];
  sort_by: string;
  sort_dir: "asc" | "desc";
  group_by?: string;
  hidden_cols: string[];
  created_at: string;
}

export const databaseApi = {
  create(pageID: string, body: { name?: string; workspace_id?: string }) {
    return apiRequest<Database>(`/v1/pages/${pageID}/databases`, {
      method: "POST",
      body,
    });
  },
  get(dbID: string) {
    return apiRequest<Database>(`/v1/databases/${dbID}`);
  },
  updateSchema(dbID: string, schema: ColumnDef[]) {
    return apiRequest<Database>(`/v1/databases/${dbID}/schema`, {
      method: "PATCH",
      body: { schema },
    });
  },
  createRow(dbID: string, body: Partial<Row>) {
    return apiRequest<Row>(`/v1/databases/${dbID}/rows`, {
      method: "POST",
      body,
    });
  },
  updateRow(dbID: string, rowID: string, values: Record<string, unknown>) {
    return apiRequest<Row>(`/v1/databases/${dbID}/rows/${rowID}`, {
      method: "PATCH",
      body: { values },
    });
  },
  deleteRow(dbID: string, rowID: string) {
    return apiRequest<{ ok: boolean }>(`/v1/databases/${dbID}/rows/${rowID}`, {
      method: "DELETE",
    });
  },
  listRows(dbID: string, viewID?: string) {
    const suffix = viewID ? `?view_id=${viewID}` : "";
    return apiRequest<Row[]>(`/v1/databases/${dbID}/rows${suffix}`);
  },
  createView(dbID: string, body: Partial<DatabaseView>) {
    return apiRequest<DatabaseView>(`/v1/databases/${dbID}/views`, {
      method: "POST",
      body,
    });
  },
  listViews(dbID: string) {
    return apiRequest<DatabaseView[]>(`/v1/databases/${dbID}/views`);
  },
  updateView(dbID: string, viewID: string, updates: Partial<DatabaseView>) {
    return apiRequest<DatabaseView>(`/v1/databases/${dbID}/views/${viewID}`, {
      method: "PATCH",
      body: updates,
    });
  },
};
