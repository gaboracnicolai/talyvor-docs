// ai-assist exists as a thin stub so the slash menu's AI commands
// have a handler to dispatch into. Phase 3 wires a real Lens-backed
// call here; Phase 2 surfaces the affordance + a toast.
//
// Each handler returns a promise<string> with the suggested text.
// Callers replace the editor selection with the result.

export interface AIRequest {
  id: string;
  selection?: string;
  context?: string;
}

export interface AIResponse {
  text: string;
}

export async function callAI(_req: AIRequest): Promise<AIResponse> {
  // Phase 2 stub: surface a placeholder so users see *something*
  // when they pick an AI command. Phase 3 swaps this for a real
  // Lens POST.
  return { text: "[AI not yet wired — Phase 3 ships the Lens integration]" };
}
