import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Search } from "lucide-react";
import { templatesApi, type TemplateCategory } from "~/api/templates";
import { useSpaces } from "~/hooks/useSpaces";
import { TemplateCard } from "~/components/TemplateCard";

interface GalleryProps {
  workspaceID: string;
  onCreated: (spaceID: string, pageID: string) => void;
}

const CATEGORY_TABS: { value?: TemplateCategory; label: string }[] = [
  { value: undefined, label: "All" },
  { value: "engineering", label: "Engineering" },
  { value: "product", label: "Product" },
  { value: "hr", label: "HR" },
  { value: "marketing", label: "Marketing" },
  { value: "operations", label: "Operations" },
  { value: "general", label: "General" },
  { value: "finance", label: "Finance" },
];

// TemplateGalleryPage renders the workspace's template library with
// category tabs and a search box. Each card "Use template" mints a
// page in the selected target space and routes the caller into it.
export function TemplateGalleryPage({ workspaceID, onCreated }: GalleryProps) {
  const qc = useQueryClient();
  const [category, setCategory] = useState<TemplateCategory | undefined>();
  const [search, setSearch] = useState("");

  const spaces = useSpaces();
  const [spaceID, setSpaceID] = useState<string>("");
  // Pin the default target to the first space whenever the list
  // resolves and the user hasn't picked one yet.
  useMemo(() => {
    if (!spaceID && spaces.data && spaces.data.length > 0) {
      setSpaceID(spaces.data[0].id);
    }
  }, [spaces.data, spaceID]);

  const list = useQuery({
    queryKey: ["templates", workspaceID, category ?? "", search],
    queryFn: () => templatesApi.list(workspaceID, { category, search }),
  });

  const use = useMutation({
    mutationFn: (templateID: string) =>
      templatesApi.use(workspaceID, templateID, { space_id: spaceID }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["templates", workspaceID] });
      onCreated(spaceID, res.page_id);
    },
  });

  const remove = useMutation({
    mutationFn: (templateID: string) => templatesApi.delete(workspaceID, templateID),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["templates", workspaceID] }),
  });

  return (
    <div className="mx-auto max-w-5xl space-y-4 p-8">
      <header>
        <h1 className="text-lg font-semibold">Template gallery</h1>
        <p className="text-xs text-muted">
          Pre-built page templates for every team. Pick one or save a page
          as a workspace template from any page's right panel.
        </p>
      </header>

      {/* Target-space picker + search */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1 rounded border border-border bg-bg px-2 py-1 text-xs">
          <Search size={12} className="text-muted" />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search templates"
            className="w-56 bg-transparent focus:outline-none"
          />
        </div>
        <div className="ml-auto flex items-center gap-1 text-xs">
          <span className="text-muted">Create in:</span>
          <select
            value={spaceID}
            onChange={(e) => setSpaceID(e.target.value)}
            className="rounded border border-border bg-bg px-1 py-1 text-xs"
          >
            {(spaces.data ?? []).map((sp) => (
              <option key={sp.id} value={sp.id}>
                {sp.icon} {sp.name}
              </option>
            ))}
          </select>
        </div>
      </div>

      {/* Category tabs */}
      <nav className="flex flex-wrap gap-1 border-b border-border pb-1">
        {CATEGORY_TABS.map((t) => (
          <button
            key={t.label}
            onClick={() => setCategory(t.value)}
            className={`rounded px-2 py-1 text-[10px] uppercase tracking-wider ${
              category === t.value
                ? "bg-accent text-bg"
                : "text-muted hover:text-text"
            }`}
          >
            {t.label}
          </button>
        ))}
      </nav>

      {list.isLoading ? (
        <p className="text-xs text-muted">Loading templates…</p>
      ) : (list.data ?? []).length === 0 ? (
        <p className="text-xs text-muted">No templates match your filters.</p>
      ) : (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {list.data!.map((t) => (
            <TemplateCard
              key={t.id}
              template={t}
              onUse={() => use.mutate(t.id)}
              onDelete={t.is_built_in ? undefined : () => remove.mutate(t.id)}
              busy={use.isPending && use.variables === t.id}
            />
          ))}
        </div>
      )}
    </div>
  );
}
