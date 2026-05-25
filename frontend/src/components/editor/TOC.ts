import type { Node as PMNode } from "prosemirror-model";

export interface TOCEntry {
  id: string;
  text: string;
  level: number;
  pos: number;
}

// extractTOC walks the ProseMirror doc and emits one entry per
// heading (level 1-3). The `id` is the slugified text — same text
// reliably yields the same id, so anchor links keep working when
// headings move around the document.
//
// Headings at level > 3 are intentionally ignored. The TOC is a
// navigation aid; nested H4+ would crowd the list more than it
// helps.
export function extractTOC(doc: PMNode): TOCEntry[] {
  const entries: TOCEntry[] = [];
  doc.descendants((node, pos) => {
    if (node.type.name !== "heading") return true;
    const level = (node.attrs?.level as number | undefined) ?? 1;
    if (level > 3) return true;
    const text = node.textContent.trim();
    if (!text) return true;
    entries.push({
      id: headingID(text),
      text,
      level,
      pos,
    });
    return true;
  });
  return dedupeIDs(entries);
}

// headingID is the canonical slug → id mapping. Stable for identical
// text, so /docs/page#auth-flow keeps resolving even after edits
// elsewhere in the doc.
export function headingID(text: string): string {
  return (
    text
      .toLowerCase()
      .normalize("NFKD")
      .replace(/[̀-ͯ]/g, "")
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "") || "heading"
  );
}

// dedupeIDs disambiguates collisions by suffixing -2, -3, ...
// Anchored links from outside the doc still work because the first
// occurrence keeps the unsuffixed slug.
function dedupeIDs(entries: TOCEntry[]): TOCEntry[] {
  const seen = new Map<string, number>();
  return entries.map((e) => {
    const n = (seen.get(e.id) ?? 0) + 1;
    seen.set(e.id, n);
    return n === 1 ? e : { ...e, id: `${e.id}-${n}` };
  });
}

// renderTOC builds the inner HTML for a static (non-React) consumer
// — used by the ProseMirror node view that paints into a DOM node
// outside React's tree. Indent steps are hardcoded so the markup
// stays portable (no Tailwind classes leaking outside React).
export function renderTOC(entries: TOCEntry[]): string {
  if (entries.length === 0) {
    return '<div class="toc-empty">No headings found.</div>';
  }
  const items = entries
    .map(
      (e) =>
        `<li style="margin-left:${(e.level - 1) * 16}px"><a href="#${e.id}">${escapeHTML(e.text)}</a></li>`,
    )
    .join("");
  return `<div class="toc-title">Table of Contents</div><ul>${items}</ul>`;
}

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}
