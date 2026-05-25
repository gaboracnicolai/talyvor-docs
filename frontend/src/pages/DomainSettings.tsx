import { useState } from "react";
import { Check, Clock, Copy, Globe, RefreshCcw, Trash2, X } from "lucide-react";
import { useCustomDomains } from "~/hooks/useCustomDomains";
import { useSpaces } from "~/hooks/useSpaces";
import { OfflineSettings } from "~/components/OfflineSettings";
import type { CustomDomain } from "~/api/customdomain";

interface SettingsProps {
  workspaceID: string;
}

// DomainSettingsPage is the workspace-level domain management UI.
// One column lists the existing domains with their status + DNS
// instructions; the add-domain form lives at the bottom so the
// flow reads top-down (your existing domains → add a new one).
export function DomainSettingsPage({ workspaceID }: SettingsProps) {
  const { domains, isLoading, create, verify, remove } = useCustomDomains(workspaceID);
  const spaces = useSpaces();

  const [newDomain, setNewDomain] = useState("");
  const [spaceID, setSpaceID] = useState<string>("");
  const [createErr, setCreateErr] = useState<string | null>(null);

  const onAdd = async () => {
    setCreateErr(null);
    if (!newDomain.trim()) return;
    try {
      await create.mutateAsync({
        domain: newDomain.trim(),
        space_id: spaceID || null,
      });
      setNewDomain("");
    } catch (e) {
      setCreateErr(e instanceof Error ? e.message : "Failed to add domain");
    }
  };

  return (
    <div className="mx-auto max-w-3xl space-y-4 p-8">
      <header>
        <h1 className="text-lg font-semibold">Custom domains</h1>
        <p className="text-xs text-muted">
          Serve a space at your own hostname (e.g. <code>docs.company.com</code>).
          Verify ownership via a DNS TXT record; SSL is handled by your reverse
          proxy or CDN.
        </p>
      </header>

      {isLoading ? (
        <p className="text-xs text-muted">Loading…</p>
      ) : domains.length === 0 ? (
        <p className="text-xs text-muted">No custom domains yet.</p>
      ) : (
        <ul className="space-y-2">
          {domains.map((d) => (
            <DomainRow
              key={d.id}
              domain={d}
              onVerify={() => verify.mutate(d.id)}
              onDelete={() => remove.mutate(d.id)}
            />
          ))}
        </ul>
      )}

      <section className="rounded border border-dashed border-border p-3 text-xs">
        <header className="mb-2 flex items-center gap-1 text-sm font-semibold">
          <Globe size={12} /> Add a domain
        </header>
        <div className="space-y-1">
          <input
            value={newDomain}
            onChange={(e) => setNewDomain(e.target.value)}
            placeholder="docs.company.com"
            className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
          />
          <select
            value={spaceID}
            onChange={(e) => setSpaceID(e.target.value)}
            className="w-full rounded border border-border bg-bg px-1 py-1 text-xs"
          >
            <option value="">— Whole workspace —</option>
            {(spaces.data ?? []).map((sp) => (
              <option key={sp.id} value={sp.id}>
                {sp.icon} {sp.name}
              </option>
            ))}
          </select>
          <button
            onClick={() => void onAdd()}
            disabled={!newDomain.trim() || create.isPending}
            className="w-full rounded bg-accent px-2 py-1 text-[10px] text-bg hover:opacity-90 disabled:opacity-40"
          >
            {create.isPending ? "Adding…" : "Add domain"}
          </button>
          {createErr ? (
            <div className="text-[10px] text-callout-error">{createErr}</div>
          ) : null}
        </div>
      </section>

      <OfflineSettings />
    </div>
  );
}

function DomainRow({
  domain,
  onVerify,
  onDelete,
}: {
  domain: CustomDomain;
  onVerify: () => void;
  onDelete: () => void;
}) {
  return (
    <li className="rounded border border-border bg-surface p-3 text-xs">
      <header className="flex items-center gap-2">
        <Globe size={11} className="text-muted" />
        <span className="font-mono text-sm">{domain.domain}</span>
        <StatusChip verified={domain.verified} ssl={domain.ssl_status} />
        <div className="ml-auto flex items-center gap-1">
          <button
            onClick={onVerify}
            className="inline-flex items-center gap-1 rounded border border-border bg-bg px-1.5 py-1 text-[10px] text-muted hover:border-accent hover:text-text"
          >
            <RefreshCcw size={10} /> Check DNS
          </button>
          <button
            onClick={onDelete}
            className="text-muted hover:text-callout-error"
            title="Delete domain"
          >
            <Trash2 size={11} />
          </button>
        </div>
      </header>
      {!domain.verified ? (
        <DNSInstructions token={domain.verify_token} />
      ) : (
        <div className="mt-1 text-[10px] text-muted">
          Pointed at this server — make sure your reverse proxy / CDN forwards
          the host header.
        </div>
      )}
    </li>
  );
}

function StatusChip({ verified, ssl }: { verified: boolean; ssl: string }) {
  if (verified) {
    return (
      <span className="inline-flex items-center gap-1 rounded border border-callout-success/40 bg-callout-success/15 px-1.5 py-0.5 text-[10px] text-callout-success">
        <Check size={10} /> {ssl === "active" ? "Active" : "Verified"}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1 rounded border border-callout-warning/40 bg-callout-warning/15 px-1.5 py-0.5 text-[10px] text-callout-warning">
      <Clock size={10} /> Pending verification
    </span>
  );
}

function DNSInstructions({ token }: { token: string }) {
  return (
    <div className="mt-2 rounded border border-border bg-bg p-2 text-[10px] text-muted">
      <div className="mb-1 font-semibold text-text">
        Add this TXT record to your DNS:
      </div>
      <div className="grid grid-cols-[80px_1fr] gap-1">
        <span>Type</span>
        <code>TXT</code>
        <span>Name</span>
        <code>@</code>
        <span>Value</span>
        <span className="flex items-center gap-1">
          <code className="truncate">{token}</code>
          <button
            onClick={() => void navigator.clipboard.writeText(token)}
            className="text-muted hover:text-text"
            title="Copy"
          >
            <Copy size={10} />
          </button>
        </span>
      </div>
      <div className="mt-1">
        Then click <strong>Check DNS</strong>. DNS can take a few minutes to
        propagate.
      </div>
    </div>
  );
}

// Re-export Lucide's X so dead-code elimination doesn't drop it
// from the bundle when future tweaks need it. Trivial cost.
export { X };
