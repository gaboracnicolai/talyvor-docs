import { useMemo } from "react";
import { lowlight } from "../extensions/code-highlight";
import type { Root, RootContent, ElementContent } from "hast";

interface CodeBlockProps {
  language?: string;
  code: string;
}

// Read-only renderer for ProseMirror code_block content. The editor
// itself highlights via decorations; this component is for places
// where we need to render code from page JSON without booting an
// EditorView (page previews, search hit snippets).
export function CodeBlock({ language, code }: CodeBlockProps) {
  const html = useMemo(() => {
    if (!code) return "";
    try {
      const tree = language
        ? lowlight.highlight(language, code)
        : lowlight.highlightAuto(code);
      return treeToHTML(tree);
    } catch {
      return escapeHTML(code);
    }
  }, [language, code]);

  return (
    <pre className="overflow-x-auto rounded-md border border-border bg-surface p-3 text-xs">
      <code
        className={`hljs language-${language ?? "plaintext"}`}
        dangerouslySetInnerHTML={{ __html: html }}
      />
    </pre>
  );
}

// treeToHTML walks the lowlight HAST tree and emits HTML strings.
// Inline-only (no nested blocks) so the output is safe inside a
// single <code> element.
function treeToHTML(tree: Root): string {
  let out = "";
  // Accept hast's full child union — lowlight only emits text +
  // element nodes, but tree.children is typed as RootContent[] so
  // the parameter type widens to match.
  const walk = (nodes: ReadonlyArray<RootContent | ElementContent>) => {
    for (const n of nodes) {
      if (n.type === "text") {
        out += escapeHTML(n.value);
      } else if (n.type === "element") {
        const cls = (n.properties?.className as string[] | undefined)?.join(" ") ?? "";
        out += `<span class="${escapeHTML(cls)}">`;
        walk(n.children);
        out += "</span>";
      }
      // Doctype / Comment are silently dropped — lowlight never
      // produces them, so we don't need a render path.
    }
  };
  walk(tree.children);
  return out;
}

function escapeHTML(s: string): string {
  return s
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}
