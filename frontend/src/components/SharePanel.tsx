import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Link2, Lock, Trash2, X } from "lucide-react";
import { permissionsApi, type AccessLevel, type Permission } from "~/api/permissions";
import { sharingApi, type ShareLink } from "~/api/sharing";

interface ShareProps {
  spaceID: string;
  pageID: string;
  workspaceID: string;
  spacePrivate?: boolean;
  open: boolean;
  onClose: () => void;
}

const ROLE_LABELS: Record<AccessLevel, string> = {
  none: "No access",
  view: "Can view",
  comment: "Can comment",
  edit: "Can edit",
  admin: "Admin",
};

const EXPIRY_PRESETS: { label: string; days: number }[] = [
  { label: "7 days", days: 7 },
  { label: "30 days", days: 30 },
  { label: "Never", days: 0 },
];

// SharePanel is the modal opened from the "Share" button. Three
// sections: per-member grants, public link generator, and a hint
// about the space-level inheritance. The host controls open/close so
// the badge press in PageView stays simple.
export function SharePanel({
  spaceID,
  pageID,
  workspaceID,
  spacePrivate,
  open,
  onClose,
}: ShareProps) {
  const qc = useQueryClient();
  const perms = useQuery({
    queryKey: ["page-permissions", pageID],
    queryFn: () => permissionsApi.listPage(spaceID, pageID),
    enabled: open,
  });
  const links = useQuery({
    queryKey: ["page-shares", pageID],
    queryFn: () => sharingApi.list(spaceID, pageID),
    enabled: open,
  });

  // Section 1 — per-member grant.
  const [memberID, setMemberID] = useState("");
  const [role, setRole] = useState<AccessLevel>("view");
  const grant = useMutation({
    mutationFn: () =>
      permissionsApi.grantPage(spaceID, pageID, {
        subject_type: "member",
        subject_id: memberID.trim(),
        access: role,
        workspace_id: workspaceID,
      }),
    onSuccess: () => {
      setMemberID("");
      qc.invalidateQueries({ queryKey: ["page-permissions", pageID] });
    },
  });
  const revokePerm = useMutation({
    mutationFn: (permID: string) => permissionsApi.revokePage(spaceID, pageID, permID),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["page-permissions", pageID] }),
  });

  // Section 2 — public link.
  const [linkAccess, setLinkAccess] = useState<AccessLevel>("view");
  const [expiresInDays, setExpiresInDays] = useState<number>(7);
  const [password, setPassword] = useState("");
  const create = useMutation({
    mutationFn: () =>
      sharingApi.create(spaceID, pageID, {
        access: linkAccess,
        expires_in_days: expiresInDays,
        password: password || undefined,
        workspace_id: workspaceID,
      }),
    onSuccess: () => {
      setPassword("");
      qc.invalidateQueries({ queryKey: ["page-shares", pageID] });
    },
  });
  const revokeLink = useMutation({
    mutationFn: (id: string) => sharingApi.revoke(spaceID, pageID, id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["page-shares", pageID] }),
  });

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center bg-black/40 pt-24"
      onClick={onClose}
    >
      <div
        className="w-full max-w-lg rounded-md border border-border bg-surface shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex items-center justify-between border-b border-border px-3 py-2">
          <div className="text-sm font-semibold">Share page</div>
          <button onClick={onClose} className="text-muted hover:text-text">
            <X size={14} />
          </button>
        </header>

        <div className="space-y-4 p-3">
          {/* Section 1 — invite a member */}
          <section>
            <h3 className="mb-1 text-xs font-semibold">Share with people</h3>
            <div className="flex items-center gap-1">
              <input
                value={memberID}
                onChange={(e) => setMemberID(e.target.value)}
                placeholder="member id"
                className="flex-1 rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
              />
              <select
                value={role}
                onChange={(e) => setRole(e.target.value as AccessLevel)}
                className="rounded border border-border bg-bg px-1 py-1 text-xs"
              >
                {(["view", "comment", "edit", "admin"] as AccessLevel[]).map((a) => (
                  <option key={a} value={a}>
                    {ROLE_LABELS[a]}
                  </option>
                ))}
              </select>
              <button
                onClick={() => grant.mutate()}
                disabled={!memberID.trim() || grant.isPending}
                className="rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90 disabled:opacity-40"
              >
                Share
              </button>
            </div>
            <ul className="mt-2 space-y-1 text-xs">
              {(perms.data ?? []).map((p) => (
                <PermRow key={p.id} p={p} onRevoke={() => revokePerm.mutate(p.id)} />
              ))}
            </ul>
          </section>

          {/* Section 2 — public link */}
          <section className="border-t border-border pt-3">
            <h3 className="mb-1 text-xs font-semibold">Share link</h3>
            <div className="flex flex-wrap items-center gap-1">
              <select
                value={linkAccess}
                onChange={(e) => setLinkAccess(e.target.value as AccessLevel)}
                className="rounded border border-border bg-bg px-1 py-1 text-xs"
              >
                <option value="view">Anyone can view</option>
                <option value="comment">Anyone can comment</option>
              </select>
              <select
                value={expiresInDays}
                onChange={(e) => setExpiresInDays(Number(e.target.value))}
                className="rounded border border-border bg-bg px-1 py-1 text-xs"
              >
                {EXPIRY_PRESETS.map((p) => (
                  <option key={p.label} value={p.days}>
                    {p.label}
                  </option>
                ))}
              </select>
              <input
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                type="password"
                placeholder="Optional password"
                className="flex-1 rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
              />
              <button
                onClick={() => create.mutate()}
                disabled={create.isPending}
                className="rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90 disabled:opacity-40"
              >
                Create link
              </button>
            </div>
            <ul className="mt-2 space-y-1 text-xs">
              {(links.data ?? []).map((l) => (
                <LinkRow key={l.id} l={l} onRevoke={() => revokeLink.mutate(l.id)} />
              ))}
            </ul>
          </section>

          {/* Section 3 — inherited space access */}
          <section className="border-t border-border pt-3 text-xs">
            <div className="flex items-center gap-1 text-muted">
              {spacePrivate ? <Lock size={11} /> : <Link2 size={11} />}
              <span>
                {spacePrivate
                  ? "This space is private. Page inherits its access list."
                  : "This space is public. All workspace members can view by default."}
              </span>
            </div>
          </section>
        </div>
      </div>
    </div>
  );
}

function PermRow({ p, onRevoke }: { p: Permission; onRevoke: () => void }) {
  return (
    <li className="flex items-center justify-between rounded border border-border px-2 py-1">
      <div className="flex items-center gap-1 truncate">
        <span className="font-mono text-[10px] text-muted">
          {p.subject_type}:{p.subject_id.slice(0, 12)}
        </span>
        <span className="rounded bg-bg px-1 py-px text-[10px]">
          {ROLE_LABELS[p.access]}
        </span>
      </div>
      <button onClick={onRevoke} className="text-muted hover:text-callout-error">
        <Trash2 size={11} />
      </button>
    </li>
  );
}

function LinkRow({ l, onRevoke }: { l: ShareLink; onRevoke: () => void }) {
  const url = `${window.location.origin}/s/${l.token}`;
  return (
    <li className="flex items-center justify-between rounded border border-border px-2 py-1">
      <div className="flex flex-1 items-center gap-2 truncate">
        {l.has_password ? <Lock size={11} className="text-muted" /> : null}
        <code className="truncate text-[10px] text-muted">{url}</code>
        <span className="rounded bg-bg px-1 py-px text-[10px]">
          {ROLE_LABELS[l.access]}
        </span>
        {l.expires_at ? (
          <span className="text-[10px] text-muted">
            exp {new Date(l.expires_at).toLocaleDateString()}
          </span>
        ) : null}
      </div>
      <button
        onClick={() => {
          void navigator.clipboard.writeText(url);
        }}
        title="Copy link"
        className="text-muted hover:text-text"
      >
        <Copy size={11} />
      </button>
      <button onClick={onRevoke} className="ml-1 text-muted hover:text-callout-error">
        <Trash2 size={11} />
      </button>
    </li>
  );
}
