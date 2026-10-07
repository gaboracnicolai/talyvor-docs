import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Link2, Lock, Trash2, Users, X } from "lucide-react";
import { APIError } from "~/api/client";
import { permissionsApi, type AccessLevel, type Permission } from "~/api/permissions";
import { sharingApi, type ShareLink } from "~/api/sharing";
import { teamsApi, type Team } from "~/api/teams";

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

// SharePanel is the modal opened from the "Share" button. Four
// sections: per-member grants, team grants (with the teams themselves),
// public link generator, and a hint about the space-level inheritance. The host controls open/close so
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

  // Section 2 — team grant. A team grant applies to whoever is on the team at the time of each
  // request, so taking someone off the team takes the access away. Only a team you manage can be
  // granted (permission.Store.Grant): its roster then stays in the hands of whoever shared.
  const teams = useQuery({
    queryKey: ["teams", workspaceID],
    queryFn: () => teamsApi.list(workspaceID),
    enabled: open,
  });
  const [teamID, setTeamID] = useState("");
  const [teamRole, setTeamRole] = useState<AccessLevel>("view");
  const grantTeam = useMutation({
    mutationFn: () =>
      permissionsApi.grantPage(spaceID, pageID, {
        subject_type: "team",
        subject_id: teamID,
        access: teamRole,
        workspace_id: workspaceID,
      }),
    onSuccess: () => {
      setTeamID("");
      qc.invalidateQueries({ queryKey: ["page-permissions", pageID] });
    },
  });
  const teamNames = new Map((teams.data ?? []).map((t) => [t.id, t.name]));

  // Section 2 — public link. Fixed at "view": the public share surface is a single GET
  // (/v1/public/s/{token}) with no write path, so "comment" was never enforceable on it. Not
  // state, because nothing can change it.
  const linkAccess: AccessLevel = "view";
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
      className="fixed inset-0 z-50 flex items-start justify-center bg-bg/70 pt-24"
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
                <PermRow
                  key={p.id}
                  p={p}
                  teamName={teamNames.get(p.subject_id)}
                  onRevoke={() => revokePerm.mutate(p.id)}
                />
              ))}
            </ul>
          </section>

          {/* Section 2 — share with a team */}
          <section className="border-t border-border pt-3">
            <h3 className="mb-1 text-xs font-semibold">Share with a team</h3>
            <div className="flex items-center gap-1">
              <select
                aria-label="Team"
                value={teamID}
                onChange={(e) => setTeamID(e.target.value)}
                className="min-w-0 flex-1 rounded border border-border bg-bg px-1 py-1 text-xs"
              >
                <option value="">Choose one of your teams</option>
                {(teams.data ?? []).filter((t) => t.can_manage).map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
              </select>
              <select
                aria-label="Team access"
                value={teamRole}
                onChange={(e) => setTeamRole(e.target.value as AccessLevel)}
                className="rounded border border-border bg-bg px-1 py-1 text-xs"
              >
                {(["view", "comment", "edit", "admin"] as AccessLevel[]).map((a) => (
                  <option key={a} value={a}>
                    {ROLE_LABELS[a]}
                  </option>
                ))}
              </select>
              <button
                onClick={() => grantTeam.mutate()}
                disabled={!teamID || grantTeam.isPending}
                className="rounded bg-accent px-2 py-1 text-xs text-bg hover:opacity-90 disabled:opacity-40"
              >
                Share
              </button>
            </div>
            <TeamManager workspaceID={workspaceID} teams={teams.data ?? []} />
          </section>

          {/* Section 3 — public link */}
          <section className="border-t border-border pt-3">
            <h3 className="mb-1 text-xs font-semibold">Share link</h3>
            <div className="flex flex-wrap items-center gap-1">
              {/*
                Share links are VIEW-ONLY, so there is no selector here any more.

                This offered "Anyone can comment", and the backend stored and echoed
                access:"comment" — but the entire public surface is one route,
                GET /v1/public/s/{token} (sharing.MountPublic). There is no public write path,
                so an unauthenticated share visitor could never comment: the option promised a
                capability the system does not have. Commenting requires the AccessComment tier
                on a real membership, which the member grants above do offer.

                Fixed by removing the option rather than the label, because implementing public
                commenting would mean an unauthenticated write path — a much larger decision
                than a dropdown.
              */}
              <span className="rounded border border-border bg-bg px-1 py-1 text-xs text-muted">
                Anyone with the link can view
              </span>
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

          {/* Section 4 — inherited space access */}
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

function PermRow({
  p,
  teamName,
  onRevoke,
}: {
  p: Permission;
  teamName?: string;
  onRevoke: () => void;
}) {
  return (
    <li className="flex items-center justify-between rounded border border-border px-2 py-1">
      <div className="flex items-center gap-1 truncate">
        {p.subject_type === "team" && teamName ? (
          <span className="flex items-center gap-1 truncate">
            <Users size={11} className="text-muted" />
            {teamName}
          </span>
        ) : (
          <span className="font-mono text-[10px] text-muted">
            {p.subject_type}:{p.subject_id.slice(0, 12)}
          </span>
        )}
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

// TeamManager lists the workspace's teams and who is on each. Anyone in the workspace can create a
// team; only its creator (can_manage) can add or remove members.
function TeamManager({ workspaceID, teams }: { workspaceID: string; teams: Team[] }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const refresh = () => qc.invalidateQueries({ queryKey: ["teams", workspaceID] });
  const fail = (e: unknown) => setError(e instanceof APIError ? e.message : "Something went wrong");
  const create = useMutation({
    mutationFn: () => teamsApi.create(workspaceID, name.trim()),
    onSuccess: () => {
      setName("");
      setError("");
      refresh();
    },
    onError: fail,
  });
  return (
    <details className="mt-2 text-xs">
      <summary className="cursor-pointer text-muted hover:text-text">
        Teams in this workspace ({teams.length})
      </summary>
      <ul className="mt-2 space-y-1">
        {teams.map((t) => (
          <TeamRow key={t.id} workspaceID={workspaceID} team={t} onChange={refresh} onError={fail} />
        ))}
      </ul>
      <div className="mt-2 flex items-center gap-1">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="New team name"
          maxLength={80}
          className="min-w-0 flex-1 rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
        />
        <button
          onClick={() => create.mutate()}
          disabled={!name.trim() || create.isPending}
          className="rounded border border-border px-2 py-1 text-xs hover:border-accent disabled:opacity-40"
        >
          Create team
        </button>
      </div>
      {error ? <p className="mt-1 text-callout-error">{error}</p> : null}
    </details>
  );
}

function TeamRow({
  workspaceID,
  team,
  onChange,
  onError,
}: {
  workspaceID: string;
  team: Team;
  onChange: () => void;
  onError: (e: unknown) => void;
}) {
  const [memberID, setMemberID] = useState("");
  const add = useMutation({
    mutationFn: () => teamsApi.addMember(workspaceID, team.id, memberID.trim()),
    onSuccess: () => {
      setMemberID("");
      onChange();
    },
    onError,
  });
  const remove = useMutation({
    mutationFn: (m: string) => teamsApi.removeMember(workspaceID, team.id, m),
    onSuccess: onChange,
    onError,
  });
  const count = team.members.length;
  return (
    <li className="rounded border border-border px-2 py-1">
      <div className="flex items-center justify-between gap-2">
        <span className="flex items-center gap-1 truncate font-semibold">
          <Users size={11} className="text-muted" />
          {team.name}
        </span>
        <span className="shrink-0 text-[10px] text-muted">
          {count} {count === 1 ? "member" : "members"}
        </span>
      </div>
      {count > 0 ? (
        <ul className="mt-1 flex flex-wrap gap-1">
          {team.members.map((m) => (
            <li key={m} className="flex items-center gap-1 rounded bg-bg px-1 py-px">
              <span className="font-mono text-[10px] text-muted">{m.slice(0, 16)}</span>
              {team.can_manage ? (
                <button
                  onClick={() => remove.mutate(m)}
                  aria-label={`Remove ${m} from ${team.name}`}
                  className="text-muted hover:text-callout-error"
                >
                  <X size={10} />
                </button>
              ) : null}
            </li>
          ))}
        </ul>
      ) : null}
      {team.can_manage ? (
        <div className="mt-1 flex items-center gap-1">
          <input
            value={memberID}
            onChange={(e) => setMemberID(e.target.value)}
            placeholder="member id"
            aria-label={`Add a member to ${team.name}`}
            className="min-w-0 flex-1 rounded border border-border bg-bg px-2 py-1 text-xs focus:border-accent focus:outline-none"
          />
          <button
            onClick={() => add.mutate()}
            disabled={!memberID.trim() || add.isPending}
            className="rounded border border-border px-2 py-1 text-xs hover:border-accent disabled:opacity-40"
          >
            Add
          </button>
        </div>
      ) : null}
    </li>
  );
}
