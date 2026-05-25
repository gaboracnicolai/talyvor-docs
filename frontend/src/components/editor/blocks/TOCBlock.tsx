import type { EditorView, NodeView } from "prosemirror-view";
import { extractTOC, renderTOC } from "../TOC";

// createTOCNodeView returns a ProseMirror NodeView that renders a
// live table of contents. The view re-paints whenever the
// surrounding document changes — ProseMirror calls update() for us;
// we trust nothing else.
//
// The node view DOM is plain HTML (no React) because the contents
// are read-only and we want to skirt React reconciliation cycles
// inside the editor tree.
export function createTOCNodeView(_node: unknown, view: EditorView): NodeView {
  const dom = document.createElement("div");
  dom.className = "toc-block";
  dom.style.cssText = [
    "background:var(--surface,#13161c)",
    "border-left:3px solid var(--accent,#f0a030)",
    "padding:12px 16px",
    "margin:12px 0",
    "max-width:480px",
    "border-radius:6px",
    "font-size:12px",
    "color:var(--text,#e6e8eb)",
  ].join(";");

  const paint = () => {
    const entries = extractTOC(view.state.doc);
    dom.innerHTML = renderTOC(entries);
    // Bind smooth scroll on every paint (cheap — handful of links).
    dom.querySelectorAll("a[href^='#']").forEach((a) => {
      a.addEventListener("click", (e) => {
        e.preventDefault();
        const id = (a as HTMLAnchorElement).getAttribute("href")?.slice(1);
        if (!id) return;
        const target = document.getElementById(id);
        if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
      });
    });
  };
  paint();

  // ProseMirror only fires NodeView.update() when the TOC node
  // itself changes. We also need to repaint when the *surrounding*
  // headings shift — for that we subscribe to a window event the
  // Editor dispatches on every doc transaction.
  const onRefresh = () => paint();
  window.addEventListener("docs:toc-refresh", onRefresh);

  return {
    dom,
    update(updated) {
      if (updated.type.name !== "toc_block") return false;
      paint();
      return true;
    },
    destroy() {
      window.removeEventListener("docs:toc-refresh", onRefresh);
    },
    // Keep ProseMirror from treating clicks inside the TOC as
    // selection events — the inline anchor handlers own them.
    stopEvent: () => true,
    ignoreMutation: () => true,
  };
}
