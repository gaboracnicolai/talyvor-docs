import { Plugin, PluginKey, EditorState } from "prosemirror-state";
import { Decoration, DecorationSet } from "prosemirror-view";
import { Node as PMNode } from "prosemirror-model";
import { common, createLowlight } from "lowlight";
import type { Root, RootContent, ElementContent } from "hast";

const lowlight = createLowlight(common);

// codeHighlightPlugin walks every code_block in the doc and decorates
// each token range with the highlight.js class lowlight returns.
// Decorations are recomputed on every doc-changing transaction.
export const codeHighlightKey = new PluginKey("codeHighlight");

export function codeHighlightPlugin(): Plugin {
  return new Plugin({
    key: codeHighlightKey,
    state: {
      init(_, state) {
        return buildDecorations(state);
      },
      apply(tr, old, _oldState, newState) {
        if (!tr.docChanged) return old;
        return buildDecorations(newState);
      },
    },
    props: {
      decorations(state) {
        return this.getState(state);
      },
    },
  });
}

// buildDecorations finds every code_block and runs lowlight against
// its plain-text content. The returned DecorationSet maps each token
// node to its (start, end) range inside the block.
function buildDecorations(state: EditorState): DecorationSet {
  const decorations: Decoration[] = [];
  state.doc.descendants((node, pos) => {
    if (node.type.name !== "code_block") return true;
    const lang = (node.attrs.language as string) || "";
    const text = node.textContent;
    if (!text) return false;
    let tree: Root;
    try {
      tree = lang
        ? lowlight.highlight(lang, text)
        : lowlight.highlightAuto(text);
    } catch {
      return false;
    }
    const offset = pos + 1; // +1 to step past the code_block's open token
    decorateChildren(tree.children, offset, decorations);
    return false;
  });
  return DecorationSet.create(state.doc, decorations);
}

// decorateChildren walks the lowlight HAST tree and emits one
// inline-decoration per element span. Text nodes don't get a
// decoration of their own — they're consumed implicitly when we
// advance the offset. We accept the broader RootContent[] (hast's
// child union includes Doctype + Comment, neither of which lowlight
// emits) so the top-level tree.children type matches without a
// cast at the call site.
function decorateChildren(
  nodes: ReadonlyArray<RootContent | ElementContent>,
  startOffset: number,
  out: Decoration[],
): number {
  let offset = startOffset;
  for (const node of nodes) {
    if (node.type === "text") {
      offset += node.value.length;
      continue;
    }
    if (node.type === "element") {
      const className = (node.properties?.className as string[] | undefined)?.join(" ") ?? "";
      const innerStart = offset;
      offset = decorateChildren(node.children, offset, out);
      if (className) {
        out.push(Decoration.inline(innerStart, offset, { class: className }));
      }
    }
    // Doctype / Comment land here and are silently skipped.
  }
  return offset;
}

// Re-export so other files don't have to know about the lowlight
// dependency directly.
export { lowlight };

// PMNode export silences unused-import in some TS settings.
export type _PMNode = PMNode;
