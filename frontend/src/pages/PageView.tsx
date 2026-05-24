import { useCallback, useEffect, useMemo, useState } from "react";
import { CheckCircle2, Eye, Sparkles, FileText, Link2 } from "lucide-react";
import { Editor } from "~/components/editor/Editor";
import { PresenceBar } from "~/components/editor/PresenceBar";
import { Input } from "~/components/ui/Input";
import { Button } from "~/components/ui/Button";
import { usePage, useUpdatePage } from "~/hooks/usePage";
import { pagesApi } from "~/api/pages";
import type { Space } from "~/api/types";
import type { PresenceInfo } from "~/hooks/useCollab";

interface PageViewProps {
  space: Space;
  pageID: string;
  readOnly?: boolean;
}

// PageView is the main authoring surface. Layout:
//   - title (h1) on top
//   - editor in the middle (auto-saves)
//   - right panel with TOC + page info + AI cost + linked issues
//   - footer with last-edited line + word count + reading time
export function PageViewPage({ space, pageID, readOnly }: PageViewProps) {
  const { data: page, isLoading } = usePage(space.id, pageID);
  const updateMutation = useUpdatePage(space.id, pageID);
  const [title, setTitle] = useState(page?.title ?? "");
  const [showPanel, setShowPanel] = useState(true);
  const [presence, setPresence] = useState<PresenceInfo[]>([]);
  const [selfClientID, setSelfClientID] = useState<string>("");

  const handlePresence = useCallback(
    (next: PresenceInfo[], clientID: string) => {
      setPresence(next);
      setSelfClientID(clientID);
    },
    [],
  );

  // Sync title state with the loaded page. We treat the title input
  // as a controlled component — saves flow through the same hook as
  // the editor body to keep persistence consistent.
  useEffect(() => {
    if (page) setTitle(page.title);
  }, [page?.id, page?.title]);

  // Record a view exactly once per page load.
  useEffect(() => {
    if (!page) return;
    void pagesApi.recordView(space.id, page.id).catch(() => undefined);
  }, [page?.id, space.id]);

  const onSaveBody = useCallback(
    (content: string, contentText: string) => {
      updateMutation.mutate({ content, content_text: contentText });
    },
    [updateMutation],
  );

  const flushTitle = useCallback(() => {
    if (!page) return;
    if (title.trim() === page.title) return;
    updateMutation.mutate({ title: title.trim() });
  }, [page, title, updateMutation]);

  const onVerify = useCallback(() => {
    if (!page) return;
    void pagesApi.verify(space.id, page.id);
  }, [page, space.id]);

  // TOC is extracted from the live page content. We do this from the
  // rendered DOM via querySelectorAll because the editor is the
  // canonical source of truth; React-level extraction would lag.
  const [toc, setTOC] = useState<{ level: number; text: string; id: string }[]>([]);
  useEffect(() => {
    if (!page) return;
    const id = setInterval(() => {
      const headings = document.querySelectorAll<HTMLElement>(
        ".prose-editor h1, .prose-editor h2, .prose-editor h3",
      );
      const items = Array.from(headings).map((el, i) => ({
        level: Number(el.tagName[1]),
        text: el.textContent || "",
        id: `toc-${i}`,
      }));
      setTOC((prev) => (jsonEq(prev, items) ? prev : items));
    }, 1500);
    return () => clearInterval(id);
  }, [page?.id]);

  const wordCount = useMemo(() => {
    if (!page) return 0;
    return page.content_text.split(/\s+/).filter(Boolean).length;
  }, [page?.content_text]);

  if (isLoading || !page) {
    return <div className="p-8 text-sm text-muted">Loading page…</div>;
  }

  return (
    <div className="flex flex-1 overflow-hidden">
      <main className="flex-1 overflow-y-auto">
        <article className="mx-auto max-w-3xl space-y-3 px-8 py-6">
          {/* icon + title row */}
          <div className="flex items-baseline gap-3">
            <span className="text-4xl">{page.icon || "📄"}</span>
            <Input
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              onBlur={flushTitle}
              placeholder="Untitled"
              className="border-0 bg-transparent px-0 text-2xl font-semibold focus:ring-0"
              readOnly={readOnly}
            />
          </div>

          {/* breadcrumb + live presence */}
          <div className="flex items-center justify-between">
            <nav className="text-[10px] text-muted">
              {space.name} {page.parent_id ? "› …" : ""}
            </nav>
            <PresenceBar presence={presence} selfClientID={selfClientID} />
          </div>

          {/* editor */}
          <Editor
            pageId={page.id}
            initialContent={page.content}
            readOnly={readOnly}
            onSave={onSaveBody}
            onPresence={handlePresence}
          />

          {/* footer */}
          <footer className="border-t border-border pt-3 text-[10px] text-muted">
            <span>
              Last edited by {page.updated_by || "unknown"} ·{" "}
              {new Date(page.updated_at).toLocaleString()}
            </span>
            <span className="ml-2">·</span>
            <span className="ml-2">
              {wordCount} words · ~{Math.max(1, Math.ceil(wordCount / 220))} min read
            </span>
          </footer>
        </article>
      </main>

      {showPanel ? (
        <aside className="w-72 shrink-0 overflow-y-auto border-l border-border bg-surface p-4 text-xs">
          <PanelSection title="Table of contents">
            {toc.length === 0 ? (
              <span className="text-muted">No headings yet</span>
            ) : (
              <div className="space-y-0.5">
                {toc.map((h, i) => (
                  <div
                    key={i}
                    style={{ paddingLeft: (h.level - 1) * 12 }}
                    className="truncate text-muted"
                  >
                    {h.text || "—"}
                  </div>
                ))}
              </div>
            )}
          </PanelSection>

          <PanelSection title="Page info">
            <div className="space-y-1 text-muted">
              <div className="flex items-center gap-1">
                <Eye size={10} />
                {page.view_count} views
              </div>
              <div className="flex items-center gap-1">
                <Sparkles size={10} className="text-accent" />
                ${page.ai_cost_usd.toFixed(2)} AI cost
              </div>
              <div className="flex items-center gap-1">
                <FileText size={10} />
                Created by {page.created_by || "unknown"}
              </div>
              {page.last_verified_at ? (
                <div className="flex items-center gap-1 text-callout-success">
                  <CheckCircle2 size={10} />
                  Verified{" "}
                  {new Date(page.last_verified_at).toLocaleDateString()}
                </div>
              ) : null}
            </div>
            <Button size="sm" variant="secondary" onClick={onVerify} className="mt-2 w-full">
              <CheckCircle2 size={10} /> Mark as verified
            </Button>
          </PanelSection>

          <PanelSection title="Linked issues">
            {(page.linked_issues ?? []).length === 0 ? (
              <span className="text-muted">None yet — embed an issue from the slash menu.</span>
            ) : (
              <div className="flex flex-wrap gap-1">
                {page.linked_issues!.map((id) => (
                  <span
                    key={id}
                    className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-0.5 font-mono text-[10px]"
                  >
                    <Link2 size={10} />
                    {id.slice(0, 8)}
                  </span>
                ))}
              </div>
            )}
          </PanelSection>

          <button
            onClick={() => setShowPanel(false)}
            className="mt-4 text-[10px] text-muted underline"
          >
            Hide panel
          </button>
        </aside>
      ) : (
        <button
          onClick={() => setShowPanel(true)}
          className="border-l border-border bg-surface px-2 py-1 text-[10px] text-muted"
        >
          Show
        </button>
      )}
    </div>
  );
}

function PanelSection({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="mb-4">
      <div className="mb-1 text-[10px] font-semibold uppercase tracking-wider text-muted">
        {title}
      </div>
      {children}
    </section>
  );
}

// Fast deep-equality for the TOC array. The poll interval re-runs the
// querySelector at 1.5s; bailing out on no-change avoids React
// re-renders when the user is just typing inside a paragraph.
function jsonEq(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}
