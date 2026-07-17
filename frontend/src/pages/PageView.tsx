import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Eye, Sparkles, FileText, X, Plus, Link2 } from "lucide-react";
import { Editor } from "~/components/editor/Editor";
import { PresenceBar } from "~/components/editor/PresenceBar";
import { IssueSearchDialog } from "~/components/editor/IssueSearchDialog";
import { IssueEmbed } from "~/components/editor/blocks/IssueEmbed";
import { FreshnessBadge } from "~/components/FreshnessBadge";
import { FreshnessPanel } from "~/components/FreshnessPanel";
import { SharePanel } from "~/components/SharePanel";
import { ExportMenu } from "~/components/ExportMenu";
import { DocStatusBadge } from "~/components/DocStatusBadge";
import { ApprovalPanel } from "~/components/ApprovalPanel";
import { LockBadge } from "~/components/LockBadge";
import { LockBanner } from "~/components/LockBanner";
import { EditingBanner } from "~/components/EditingBanner";
import { VersionHistory } from "~/components/VersionHistory";
import { usePageLock } from "~/hooks/usePageLock";
import { useEditSession } from "~/hooks/useEditSession";
import { CommentStatsBar } from "~/components/CommentStatsBar";
import { CommentsPanel } from "~/components/CommentsPanel";
import { ChangelogView } from "~/components/changelog/ChangelogView";
import { Input } from "~/components/ui/Input";
import { Button } from "~/components/ui/Button";
import { usePage, useUpdatePage } from "~/hooks/usePage";
import { pagesApi } from "~/api/pages";
import { linksApi } from "~/api/links";
import { freshnessApi } from "~/api/freshness";
import { analyticsApi } from "~/api/analytics";
import { templatesApi, type TemplateCategory } from "~/api/templates";
import { pushRecentPage } from "~/hooks/useSearch";
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

  // Record a view exactly once per page load, and remember it as
  // a recent for the SearchModal's empty state.
  useEffect(() => {
    if (!page) return;
    void pagesApi.recordView(space.id, page.id).catch(() => undefined);
    pushRecentPage({
      page_id: page.id,
      page_title: page.title,
      space_name: space.name || "",
      url: `/spaces/${space.id}/pages/${page.id}`,
    });
  }, [page?.id, space.id, space.name, page?.title]);

  // Phase-7 view-duration tracker. We start a wall-clock on mount
  // (and on every page navigation) and POST the elapsed seconds on
  // unmount + beforeunload. The server drops views under 3s so
  // accidental clicks don't pollute analytics. Self-views (the
  // page author) are still recorded — the server handles the
  // "never read" exclusion separately.
  const viewStart = useRef<number>(Date.now());
  useEffect(() => {
    if (!page) return;
    viewStart.current = Date.now();
    const flush = () => {
      const seconds = Math.round((Date.now() - viewStart.current) / 1000);
      if (seconds < 3) return;
      const viewerID = localStorage.getItem("docs_member_id") || "anonymous";
      const viewerName = localStorage.getItem("docs_member_name") || "";
      void analyticsApi
        .recordView(space.id, page.id, {
          viewer_id: viewerID,
          viewer_name: viewerName,
          duration_sec: seconds,
          workspace_id: page.workspace_id,
        })
        .catch(() => undefined);
    };
    window.addEventListener("beforeunload", flush);
    return () => {
      flush();
      window.removeEventListener("beforeunload", flush);
    };
  }, [page?.id, page?.workspace_id, space.id]);

  // Freshness query + popover open state.
  const freshness = useQuery({
    queryKey: ["freshness", page?.id],
    queryFn: () => freshnessApi.forPage(space.id, page!.id),
    enabled: !!page,
    staleTime: 60_000,
  });
  const [freshnessOpen, setFreshnessOpen] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);

  // Page locks. Returns the live state + lock/unlock mutations so
  // the header badge, banner, and editor read-only flag all stay
  // in sync without separate queries.
  const lockHook = usePageLock(space.id, pageID);
  const lockedByOther =
    !!lockHook.state?.locked && !lockHook.lockedByMe;

  // Single-writer edit session (Option A). Automatic sibling of the manual lock: it acquires the
  // writer slot when an EDITABLE page opens, heartbeats while open, and releases on unmount. A
  // non-holder falls to read-only (folded into the Editor's readOnly below) and sees the banner.
  // autoAcquire is gated so approved / prop-read-only pages never grab the slot — composes WITH
  // the manual lock + approval, never replaces them.
  const editSession = useEditSession(space.id, pageID, {
    autoAcquire: !readOnly && !!page && page.doc_status !== "approved",
  });
  const heldByOther = editSession.heldByOther;

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

  // TOC is read from the rendered DOM via querySelectorAll. The
  // ProseMirror heading-anchors plugin stamps each heading with a
  // stable `id`, so we can reuse those IDs for the right-panel
  // links + reuse the same IDs for the IntersectionObserver-driven
  // active highlight below.
  const [toc, setTOC] = useState<{ level: number; text: string; id: string }[]>([]);
  const [activeHeadingID, setActiveHeadingID] = useState<string>("");
  useEffect(() => {
    if (!page) return;
    const rescan = () => {
      const headings = document.querySelectorAll<HTMLElement>(
        ".prose-editor h1, .prose-editor h2, .prose-editor h3",
      );
      const items = Array.from(headings).map((el) => ({
        level: Number(el.tagName[1]),
        text: el.textContent || "",
        // The plugin stamps `id` via decorations. Fall back to a
        // synthetic id if anchors haven't painted yet — the
        // observer re-runs on the next refresh.
        id: el.id || `toc-${el.textContent?.toLowerCase().replace(/\s+/g, "-")}`,
      }));
      setTOC((prev) => (jsonEq(prev, items) ? prev : items));
    };
    rescan();
    const id = setInterval(rescan, 1500);
    // Listen for explicit refresh events from the Editor — keeps
    // the panel responsive to edits without waiting for the next
    // poll tick.
    window.addEventListener("docs:toc-refresh", rescan);
    return () => {
      clearInterval(id);
      window.removeEventListener("docs:toc-refresh", rescan);
    };
  }, [page?.id]);

  // IntersectionObserver — highlights the entry whose heading is
  // closest to the top of the viewport. We track every heading the
  // TOC knows about and pick the most-visible one.
  useEffect(() => {
    if (toc.length === 0) return;
    const elements = toc
      .map((h) => document.getElementById(h.id))
      .filter((el): el is HTMLElement => !!el);
    if (elements.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        if (visible.length > 0) {
          setActiveHeadingID((visible[0].target as HTMLElement).id);
        }
      },
      { rootMargin: "0px 0px -60% 0px" },
    );
    elements.forEach((el) => observer.observe(el));
    return () => observer.disconnect();
  }, [toc]);

  const wordCount = useMemo(() => {
    if (!page) return 0;
    return page.content_text.split(/\s+/).filter(Boolean).length;
  }, [page?.content_text]);

  if (isLoading || !page) {
    return <div className="p-8 text-sm text-muted">Loading page…</div>;
  }

  // Specialised page types render their own surface — the editor is
  // hidden behind a typed branch so we can introduce more page
  // types (templates-as-pages, dashboards, etc.) the same way.
  if (page.page_type === "changelog") {
    return (
      <main className="flex-1 overflow-y-auto">
        <ChangelogView page={page} spaceID={space.id} />
      </main>
    );
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

          {/* breadcrumb + freshness + live presence */}
          <div className="relative flex items-center justify-between">
            <nav className="text-[10px] text-muted">
              {space.name} {page.parent_id ? "› …" : ""}
            </nav>
            <div className="flex items-center gap-2">
              <FreshnessBadge
                status={freshness.data?.status ?? "unknown"}
                daysSinceEdit={freshness.data?.days_since_edit}
                onClick={() => setFreshnessOpen((v) => !v)}
              />
              <button
                onClick={() => setShareOpen(true)}
                className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-0.5 text-[10px] text-muted hover:border-accent hover:text-text"
                title="Share this page"
              >
                <Link2 size={10} /> Share
              </button>
              <ExportMenu spaceID={space.id} pageID={page.id} />
              <DocStatusBadge status={page.doc_status ?? "draft"} />
              <CommentStatsBar spaceID={space.id} pageID={page.id} />
              <LockBadge
                state={lockHook.state}
                lockedByMe={lockHook.lockedByMe}
                onLock={() => lockHook.lock.mutate()}
                onUnlock={() => lockHook.unlock.mutate({})}
                busy={lockHook.lock.isPending || lockHook.unlock.isPending}
              />
              <PresenceBar presence={presence} selfClientID={selfClientID} />
            </div>
            {freshnessOpen ? (
              <FreshnessPanel
                spaceID={space.id}
                pageID={page.id}
                report={freshness.data ?? null}
                isLoading={freshness.isLoading}
                onClose={() => setFreshnessOpen(false)}
              />
            ) : null}
          </div>
          <SharePanel
            spaceID={space.id}
            pageID={page.id}
            workspaceID={page.workspace_id}
            spacePrivate={(space as { private?: boolean }).private}
            open={shareOpen}
            onClose={() => setShareOpen(false)}
          />

          {/* Approved pages are read-only until the editor explicitly
              moves them back to draft. The banner above the editor
              explains the locked state and links out to the right
              panel for the workflow controls. */}
          {page.doc_status === "approved" ? (
            <div className="rounded border border-callout-success/40 bg-callout-success/10 px-2 py-1 text-[10px] text-callout-success">
              This page is approved. Editing is locked — request a new
              review to make changes.
            </div>
          ) : null}

          <LockBanner
            state={lockHook.state}
            lockedByMe={lockHook.lockedByMe}
            onUnlock={() => lockHook.unlock.mutate({})}
          />

          <EditingBanner
            flags={editSession}
            onTakeover={() => editSession.takeover.mutate()}
            takingOver={editSession.takeover.isPending}
          />

          {/* editor */}
          <Editor
            pageId={page.id}
            workspaceId={page.workspace_id}
            initialContent={page.content}
            readOnly={
              readOnly || page.doc_status === "approved" || lockedByOther || heldByOther
            }
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
                {toc.map((h, i) => {
                  const active = h.id === activeHeadingID;
                  return (
                    <button
                      key={i}
                      onClick={() => {
                        const target = document.getElementById(h.id);
                        if (target) {
                          target.scrollIntoView({ behavior: "smooth", block: "start" });
                        }
                      }}
                      style={{ paddingLeft: (h.level - 1) * 12 }}
                      className={`block w-full truncate text-left ${
                        active ? "text-accent" : "text-muted hover:text-text"
                      }`}
                    >
                      {h.text || "—"}
                    </button>
                  );
                })}
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
            <SaveAsTemplateSection
              pageID={page.id}
              workspaceID={page.workspace_id}
            />
          </PanelSection>

          <PanelSection title="Approval">
            <ApprovalPanel
              spaceID={space.id}
              pageID={page.id}
              docStatus={page.doc_status ?? "draft"}
            />
          </PanelSection>

          <PanelSection title="Comments">
            <CommentsPanel spaceID={space.id} pageID={page.id} />
          </PanelSection>

          <PanelSection title="Version history">
            {/* VersionHistory invalidates the ["page", space, page] query on restore itself, so
                the page refetches without a host callback. */}
            <VersionHistory spaceID={space.id} pageID={page.id} />
          </PanelSection>

          <LinkedIssuesSection pageID={page.id} workspaceID={page.workspace_id} />
          {page.ai_cost_usd > 0 ? (
            <PanelSection title="AI cost">
              <div className="flex items-center gap-1 text-accent">
                <Sparkles size={10} />✨ AI writing cost: ${page.ai_cost_usd.toFixed(2)}
              </div>
              <div className="mt-0.5 text-muted">
                Includes Lens writing + Track implementation spend.
              </div>
            </PanelSection>
          ) : null}

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

// LinkedIssuesSection lists the Track issues attached to this page
// — both embedded inline (via the slash command) and manually
// linked from the panel "Link issue" affordance.
function LinkedIssuesSection({
  pageID,
  workspaceID,
}: {
  pageID: string;
  workspaceID: string;
}) {
  const qc = useQueryClient();
  const links = useQuery({
    queryKey: ["page-links", pageID],
    queryFn: () => linksApi.list(pageID),
  });
  const create = useMutation({
    mutationFn: (issueID: string) =>
      linksApi.create(pageID, {
        issue_id: issueID,
        workspace_id: workspaceID,
        link_type: "mention",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["page-links", pageID] }),
  });
  const remove = useMutation({
    mutationFn: (issueID: string) => linksApi.remove(pageID, issueID),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["page-links", pageID] }),
  });
  const [picker, setPicker] = useState(false);

  return (
    <PanelSection title="Linked issues">
      {links.isLoading ? (
        <span className="text-muted">Loading…</span>
      ) : (links.data ?? []).length === 0 ? (
        <span className="text-muted">
          None yet — embed an issue from the slash menu or click + below.
        </span>
      ) : (
        <div className="flex flex-wrap gap-1">
          {links.data!.map((l) => (
            <span key={l.id} className="inline-flex items-center gap-0.5">
              <IssueEmbed issueID={l.issue_id} />
              <button
                onClick={() => remove.mutate(l.issue_id)}
                className="text-muted hover:text-callout-error"
                title="Remove link"
              >
                <X size={10} />
              </button>
            </span>
          ))}
        </div>
      )}
      <button
        onClick={() => setPicker(true)}
        className="mt-2 flex w-full items-center justify-center gap-1 rounded border border-dashed border-border py-1 text-[10px] text-muted hover:border-accent hover:text-text"
      >
        <Plus size={10} /> Link issue
      </button>
      <IssueSearchDialog
        open={picker}
        onPick={(issue) => {
          create.mutate(issue.id);
          setPicker(false);
        }}
        onClose={() => setPicker(false)}
      />
    </PanelSection>
  );
}

// SaveAsTemplateSection is the right-panel affordance that turns a
// page into a reusable workspace template. We open a small inline
// form rather than a modal so the user can fill it in without
// losing the editor's scroll position.
function SaveAsTemplateSection({
  pageID,
  workspaceID,
}: {
  pageID: string;
  workspaceID: string;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [category, setCategory] = useState<TemplateCategory>("general");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<string>("");

  const submit = async () => {
    if (!name.trim()) return;
    setBusy(true);
    setStatus("");
    try {
      await templatesApi.fromPage(workspaceID, {
        page_id: pageID,
        name: name.trim(),
        description: description.trim(),
        category,
      });
      setStatus("Saved.");
      setName("");
      setDescription("");
      setOpen(false);
    } catch {
      setStatus("Couldn't save template.");
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <button
        onClick={() => setOpen(true)}
        className="mt-2 w-full rounded border border-border bg-bg px-2 py-1 text-[10px] text-muted hover:border-accent hover:text-text"
      >
        + Save as template
      </button>
    );
  }
  return (
    <div className="mt-2 space-y-1 rounded border border-dashed border-border p-2">
      <input
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="Template name"
        className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
      />
      <input
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        placeholder="Description"
        className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
      />
      <select
        value={category}
        onChange={(e) => setCategory(e.target.value as TemplateCategory)}
        className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
      >
        {[
          "engineering",
          "product",
          "hr",
          "marketing",
          "finance",
          "operations",
          "general",
        ].map((c) => (
          <option key={c} value={c}>
            {c}
          </option>
        ))}
      </select>
      <div className="flex items-center gap-1">
        <button
          onClick={() => void submit()}
          disabled={busy || !name.trim()}
          className="flex-1 rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
        >
          {busy ? "Saving…" : "Save"}
        </button>
        <button
          onClick={() => setOpen(false)}
          className="rounded border border-border px-2 py-1 text-[10px] text-muted hover:text-text"
        >
          Cancel
        </button>
      </div>
      {status ? <div className="text-[10px] text-muted">{status}</div> : null}
    </div>
  );
}
