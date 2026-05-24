import { useState } from "react";
import { Home, FolderOpen, Search, Plus, ChevronRight, ChevronDown, FileText } from "lucide-react";
import clsx from "clsx";
import { useSpaces, useCreateSpace } from "~/hooks/useSpaces";
import { usePages } from "~/hooks/usePage";
import type { Page, Space } from "~/api/types";
import { Input } from "~/components/ui/Input";

interface SidebarProps {
  // Route surface lives in App.tsx; the sidebar just calls these
  // navigators with the chosen target.
  onHome: () => void;
  onOpenSpace: (space: Space) => void;
  onOpenPage: (space: Space, page: Page) => void;
  activeSpaceID: string | null;
  activePageID: string | null;
}

export function Sidebar({
  onHome,
  onOpenSpace,
  onOpenPage,
  activeSpaceID,
  activePageID,
}: SidebarProps) {
  const spaces = useSpaces();
  const create = useCreateSpace();
  const [newSpaceName, setNewSpaceName] = useState("");

  return (
    <aside className="flex h-screen w-64 shrink-0 flex-col border-r border-border bg-surface">
      <div className="flex h-12 items-center gap-2 border-b border-border px-3">
        <div className="flex h-6 w-6 items-center justify-center rounded bg-accent text-bg">
          <span className="font-mono text-xs font-bold">T</span>
        </div>
        <span className="text-sm font-semibold">Talyvor Docs</span>
      </div>

      <nav className="flex-1 overflow-y-auto p-2">
        <button
          onClick={onHome}
          className="mb-2 flex w-full items-center gap-2 rounded px-2 py-1.5 text-sm text-muted hover:bg-bg hover:text-text"
        >
          <Home size={14} />
          Home
        </button>

        <div className="mb-1 flex items-center justify-between px-2 text-[10px] font-semibold uppercase tracking-wider text-muted">
          <span>Spaces</span>
          <Search size={10} />
        </div>

        {spaces.isLoading ? (
          <div className="px-2 py-1 text-xs text-muted">Loading…</div>
        ) : (spaces.data ?? []).length === 0 ? (
          <div className="px-2 py-1 text-xs text-muted">No spaces yet.</div>
        ) : (
          spaces.data!.map((sp) => (
            <SpaceRow
              key={sp.id}
              space={sp}
              active={activeSpaceID === sp.id}
              activePageID={activePageID}
              onOpenSpace={() => onOpenSpace(sp)}
              onOpenPage={(p) => onOpenPage(sp, p)}
            />
          ))
        )}

        <div className="mt-3 space-y-1 rounded-md border border-dashed border-border p-2">
          <Input
            placeholder="New space name"
            value={newSpaceName}
            onChange={(e) => setNewSpaceName(e.target.value)}
            className="h-7 text-xs"
          />
          <button
            onClick={() => {
              if (!newSpaceName.trim()) return;
              create.mutate(
                { name: newSpaceName.trim() },
                {
                  onSuccess: () => setNewSpaceName(""),
                },
              );
            }}
            className="flex w-full items-center justify-center gap-1 rounded bg-bg px-2 py-1 text-xs text-muted hover:text-text"
          >
            <Plus size={10} /> Create space
          </button>
        </div>
      </nav>
    </aside>
  );
}

function SpaceRow({
  space,
  active,
  activePageID,
  onOpenSpace,
  onOpenPage,
}: {
  space: Space;
  active: boolean;
  activePageID: string | null;
  onOpenSpace: () => void;
  onOpenPage: (page: Page) => void;
}) {
  const [open, setOpen] = useState(active);
  const pages = usePages(open ? space.id : null);

  return (
    <div>
      <div
        className={clsx(
          "flex w-full items-center gap-1 rounded px-2 py-1 text-sm",
          active ? "bg-bg text-text" : "text-muted hover:bg-bg hover:text-text",
        )}
      >
        <button onClick={() => setOpen((o) => !o)} className="text-muted">
          {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        </button>
        <button onClick={onOpenSpace} className="flex flex-1 items-center gap-1 truncate text-left">
          <span>{space.icon}</span>
          <span className="truncate">{space.name}</span>
        </button>
      </div>
      {open ? (
        <div className="ml-5 mt-0.5 space-y-0.5">
          {pages.isLoading ? (
            <div className="px-2 py-0.5 text-[10px] text-muted">Loading…</div>
          ) : (pages.data ?? []).length === 0 ? (
            <div className="px-2 py-0.5 text-[10px] text-muted">No pages</div>
          ) : (
            (pages.data ?? []).map((p) => (
              <button
                key={p.id}
                onClick={() => onOpenPage(p)}
                className={clsx(
                  "flex w-full items-center gap-1 rounded px-2 py-0.5 text-left text-xs",
                  activePageID === p.id
                    ? "bg-bg text-text"
                    : "text-muted hover:bg-bg/60 hover:text-text",
                )}
              >
                <FileText size={10} />
                <span className="truncate">{p.icon ? `${p.icon} ${p.title}` : p.title}</span>
              </button>
            ))
          )}
          <FolderFooter />
        </div>
      ) : null}
    </div>
  );
}

function FolderFooter() {
  return (
    <div className="px-2 py-0.5 text-[10px] text-muted">
      <FolderOpen size={10} className="mr-1 inline" /> Drag to reorder (Phase 3)
    </div>
  );
}
