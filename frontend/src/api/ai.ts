import { apiRequest } from "./client";

export type AITransform = "summarize" | "grammar" | "shorter" | "longer";

export interface AISource {
  title: string;
  url: string;
}

export interface AIAskResponse {
  answer: string;
  sources: AISource[];
}

export const aiApi = {
  write(workspaceID: string, body: { prompt: string; context?: string; page_id?: string }) {
    return apiRequest<{ text: string }>(`/v1/workspaces/${workspaceID}/ai/write`, {
      method: "POST",
      body,
    });
  },
  transform(
    workspaceID: string,
    body: { action: AITransform; text: string; page_id?: string },
  ) {
    return apiRequest<{ text: string }>(`/v1/workspaces/${workspaceID}/ai/transform`, {
      method: "POST",
      body,
    });
  },
  translate(
    workspaceID: string,
    body: { text: string; language: string; page_id?: string },
  ) {
    return apiRequest<{ text: string }>(`/v1/workspaces/${workspaceID}/ai/translate`, {
      method: "POST",
      body,
    });
  },
  ask(workspaceID: string, body: { question: string }) {
    return apiRequest<AIAskResponse>(`/v1/workspaces/${workspaceID}/ai/ask`, {
      method: "POST",
      body,
    });
  },
  suggestTitle(
    workspaceID: string,
    body: { content: string; page_id?: string },
  ) {
    return apiRequest<{ title: string }>(`/v1/workspaces/${workspaceID}/ai/suggest-title`, {
      method: "POST",
      body,
    });
  },
};
