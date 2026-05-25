import { useState } from "react";
import { useChangelog } from "~/hooks/useChangelog";
import type { Page } from "~/api/types";
import { EntryCard } from "./EntryCard";
import { GeneratePanel } from "./GeneratePanel";

interface ViewProps {
  page: Page;
  spaceID: string;
}

// ChangelogView is the read-write surface for a page whose
// page_type === "changelog". Renders the entries as a timeline,
// auto-grouping by version, with a separate panel for generating
// new entries from Track issues. Editor-side flows (write a draft
// by hand) hit the standard create mutation.
export function ChangelogView({ page, spaceID }: ViewProps) {
  const { entries, isLoading, generate, publish, remove } = useChangelog(
    spaceID,
    page.id,
  );
  const [filter, setFilter] = useState<"all" | "published" | "draft">("all");

  const filtered = entries.filter((e) => {
    if (filter === "published") return !!e.published_at;
    if (filter === "draft") return !e.published_at;
    return true;
  });

  return (
    <div className="mx-auto max-w-3xl space-y-4 p-8">
      <header>
        <h1 className="text-lg font-semibold">
          {page.icon ? `${page.icon} ` : "📋 "}
          {page.title || "Changelog"}
        </h1>
        <p className="text-xs text-muted">
          Release notes auto-generated from Track issues, or written
          by hand. Published entries surface in the public RSS feed.
        </p>
      </header>

      <GeneratePanel
        busy={generate.isPending}
        onGenerate={({ version, issueIDs }) => {
          if (!version || issueIDs.length === 0) return;
          generate.mutate({
            version,
            issue_ids: issueIDs,
            workspace_id: page.workspace_id,
          });
        }}
      />

      <nav className="flex items-center gap-1 border-b border-border pb-1 text-[10px] uppercase tracking-wider">
        {(["all", "published", "draft"] as const).map((f) => (
          <button
            key={f}
            onClick={() => setFilter(f)}
            className={`rounded px-1.5 py-0.5 ${
              filter === f ? "bg-accent text-bg" : "text-muted hover:text-text"
            }`}
          >
            {f}
          </button>
        ))}
      </nav>

      {isLoading ? (
        <p className="text-xs text-muted">Loading…</p>
      ) : filtered.length === 0 ? (
        <p className="text-xs text-muted">No entries yet.</p>
      ) : (
        <div className="space-y-2">
          {filtered.map((e) => (
            <EntryCard
              key={e.id}
              entry={e}
              onPublish={(id) => publish.mutate(id)}
              onDelete={(id) => remove.mutate(id)}
            />
          ))}
        </div>
      )}
    </div>
  );
}
