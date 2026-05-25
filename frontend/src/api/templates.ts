import { apiRequest, qs } from "./client";

export type TemplateCategory =
  | "engineering"
  | "product"
  | "hr"
  | "marketing"
  | "finance"
  | "operations"
  | "general";

export interface LibraryTemplate {
  id: string;
  name: string;
  description: string;
  category: TemplateCategory;
  icon: string;
  tags: string[];
  content: string;
  content_text: string;
  is_built_in: boolean;
  workspace_id?: string | null;
  created_by?: string | null;
  use_count: number;
  created_at: string;
}

export const templatesApi = {
  list(workspaceID: string, opts: { category?: TemplateCategory; search?: string } = {}) {
    return apiRequest<LibraryTemplate[]>(
      `/v1/workspaces/${workspaceID}/template-library${qs({
        category: opts.category,
        search: opts.search,
      })}`,
    );
  },
  fromPage(workspaceID: string, body: {
    page_id: string;
    name: string;
    description?: string;
    category?: TemplateCategory;
  }) {
    return apiRequest<LibraryTemplate>(
      `/v1/workspaces/${workspaceID}/template-library/from-page`,
      { method: "POST", body },
    );
  },
  use(workspaceID: string, templateID: string, body: { space_id: string }) {
    return apiRequest<{ page_id: string; page_url: string }>(
      `/v1/workspaces/${workspaceID}/template-library/${templateID}/use`,
      { method: "POST", body },
    );
  },
  delete(workspaceID: string, templateID: string) {
    return apiRequest<{ ok: boolean }>(
      `/v1/workspaces/${workspaceID}/template-library/${templateID}`,
      { method: "DELETE" },
    );
  },
};
