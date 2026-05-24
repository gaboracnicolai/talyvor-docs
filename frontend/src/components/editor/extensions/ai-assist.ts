// ai-assist routes slash-menu AI commands to the backend AI engine,
// which proxies through Lens. The slash menu plugin dispatches a
// "docs:ai-command" CustomEvent; the Editor listens and calls
// callAI() with the chosen action + the surrounding selection.
//
// On error, callers should show a toast like
//   "AI unavailable. Is Lens configured?"
// and leave the editor selection untouched.

import { aiApi, type AITransform } from "~/api/ai";

export type AIAction =
  | "ai-write"
  | "ai-summarize"
  | "ai-grammar"
  | "ai-shorter"
  | "ai-longer"
  | "ai-translate";

export interface AIRequest {
  action: AIAction;
  text: string;
  context?: string;
  workspaceId: string;
  pageId?: string;
  // For ai-translate: the target language. UI prompts for this.
  language?: string;
  // For ai-write: the user's prompt.
  prompt?: string;
}

export interface AIResponse {
  text: string;
}

const transformMap: Record<string, AITransform> = {
  "ai-summarize": "summarize",
  "ai-grammar": "grammar",
  "ai-shorter": "shorter",
  "ai-longer": "longer",
};

// callAI fires the correct backend endpoint for the given action.
// Throws on any failure — callers (the Editor) catch and toast.
export async function callAI(req: AIRequest): Promise<AIResponse> {
  if (req.action === "ai-write") {
    const prompt = req.prompt ?? req.text;
    const res = await aiApi.write(req.workspaceId, {
      prompt,
      context: req.context ?? "",
      page_id: req.pageId,
    });
    return { text: res.text };
  }
  if (req.action === "ai-translate") {
    const language = req.language ?? "Spanish";
    const res = await aiApi.translate(req.workspaceId, {
      text: req.text,
      language,
      page_id: req.pageId,
    });
    return { text: res.text };
  }
  const action = transformMap[req.action];
  if (!action) {
    throw new Error(`unknown ai action: ${req.action}`);
  }
  const res = await aiApi.transform(req.workspaceId, {
    action,
    text: req.text,
    page_id: req.pageId,
  });
  return { text: res.text };
}
