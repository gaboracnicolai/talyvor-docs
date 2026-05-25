import { useEffect, useState } from "react";
import { CloudOff, RefreshCcw, Trash2 } from "lucide-react";
import { clearCache, estimateCache, getSyncStatus } from "~/lib/offlinedb";
import { syncManager } from "~/lib/sync";

// OfflineSettings is the workspace-settings section that exposes
// the offline cache to the user: storage estimate, last-sync
// timestamp, "Sync now" + "Clear cache" buttons.
export function OfflineSettings() {
  const [usage, setUsage] = useState({ usage: 0, quota: 0 });
  const [lastSync, setLastSync] = useState<number | null>(null);
  const [pending, setPending] = useState(0);
  const [busy, setBusy] = useState(false);

  const refresh = async () => {
    try {
      setUsage(await estimateCache());
      setLastSync((await getSyncStatus<number>("last_sync")) ?? null);
    } catch {
      // IndexedDB unavailable (Safari private mode) — show zeros.
    }
  };

  useEffect(() => {
    void refresh();
    const unsub = syncManager.subscribe((s) => setPending(s.pending));
    return unsub;
  }, []);

  return (
    <section className="rounded border border-border bg-surface p-3 text-xs">
      <header className="mb-2 flex items-center gap-1 text-sm font-semibold">
        <CloudOff size={12} /> Offline & cache
      </header>
      <dl className="grid grid-cols-[140px_1fr] gap-1 text-muted">
        <dt>Cached storage</dt>
        <dd>
          {formatBytes(usage.usage)} of {formatBytes(usage.quota) || "—"}
        </dd>
        <dt>Last synced</dt>
        <dd>{lastSync ? new Date(lastSync).toLocaleString() : "never"}</dd>
        <dt>Pending changes</dt>
        <dd>{pending}</dd>
      </dl>
      <div className="mt-2 flex items-center gap-1">
        <button
          onClick={async () => {
            setBusy(true);
            try {
              await syncManager.syncQueue();
              await refresh();
            } finally {
              setBusy(false);
            }
          }}
          disabled={busy}
          className="inline-flex items-center gap-1 rounded border border-border bg-bg px-2 py-1 text-[10px] text-muted hover:border-accent hover:text-text disabled:opacity-40"
        >
          <RefreshCcw size={10} className={busy ? "animate-spin" : ""} />
          Sync now
        </button>
        <button
          onClick={async () => {
            if (!confirm("Clear the offline cache? Unsynced changes will still flush on reconnect."))
              return;
            await clearCache();
            await refresh();
          }}
          className="inline-flex items-center gap-1 rounded border border-border bg-bg px-2 py-1 text-[10px] text-muted hover:border-callout-error hover:text-callout-error"
        >
          <Trash2 size={10} /> Clear cache
        </button>
      </div>
    </section>
  );
}

function formatBytes(b: number): string {
  if (b === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  let i = 0;
  let n = b;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(1)} ${units[i]}`;
}
