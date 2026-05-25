import { useEffect, useState } from "react";
import { CloudOff, RefreshCcw, CheckCircle2 } from "lucide-react";
import { syncManager } from "~/lib/sync";

interface IndicatorState {
  online: boolean;
  syncing: boolean;
  pending: number;
  lastSyncedAt?: number;
}

// OfflineIndicator is the slim banner the App always mounts. It
// only renders when there's something worth showing — silent when
// everything is online and clean.
export function OfflineIndicator() {
  const [state, setState] = useState<IndicatorState>({
    online: typeof navigator !== "undefined" ? navigator.onLine !== false : true,
    syncing: false,
    pending: 0,
  });
  const [showSyncedToast, setShowSyncedToast] = useState(false);

  // Subscribe to SyncManager state + navigator online events.
  useEffect(() => {
    const unsub = syncManager.subscribe((s) => {
      setState((prev) => {
        const next = {
          online: typeof navigator !== "undefined" ? navigator.onLine !== false : true,
          syncing: s.syncing,
          pending: s.pending,
          lastSyncedAt: s.lastSyncAt,
        };
        // Flash a "synced" toast when we transition from
        // pending > 0 → pending === 0 while online.
        if (prev.pending > 0 && next.pending === 0 && next.online) {
          setShowSyncedToast(true);
        }
        return next;
      });
    });
    const onlineHandler = () => setState((s) => ({ ...s, online: true }));
    const offlineHandler = () => setState((s) => ({ ...s, online: false }));
    window.addEventListener("online", onlineHandler);
    window.addEventListener("offline", offlineHandler);
    return () => {
      unsub();
      window.removeEventListener("online", onlineHandler);
      window.removeEventListener("offline", offlineHandler);
    };
  }, []);

  // Auto-hide the synced confirmation after 3s.
  useEffect(() => {
    if (!showSyncedToast) return;
    const id = setTimeout(() => setShowSyncedToast(false), 3000);
    return () => clearTimeout(id);
  }, [showSyncedToast]);

  if (!state.online) {
    return (
      <Banner tone="warning">
        <CloudOff size={11} />
        Offline — showing cached content
        {state.pending > 0 ? (
          <span className="ml-1 opacity-80">· {state.pending} change{state.pending === 1 ? "" : "s"} queued</span>
        ) : null}
      </Banner>
    );
  }
  if (state.syncing) {
    return (
      <Banner tone="info">
        <RefreshCcw size={11} className="animate-spin" />
        Syncing {state.pending} change{state.pending === 1 ? "" : "s"}…
      </Banner>
    );
  }
  if (state.pending > 0) {
    return (
      <Banner tone="info">
        <RefreshCcw size={11} />
        {state.pending} change{state.pending === 1 ? "" : "s"} queued
        <button
          onClick={() => void syncManager.syncQueue()}
          className="ml-2 underline opacity-80 hover:opacity-100"
        >
          Sync now
        </button>
      </Banner>
    );
  }
  if (showSyncedToast) {
    return (
      <Banner tone="success">
        <CheckCircle2 size={11} />
        All changes synced
      </Banner>
    );
  }
  return null;
}

function Banner({
  tone,
  children,
}: {
  tone: "warning" | "info" | "success";
  children: React.ReactNode;
}) {
  const classes =
    tone === "warning"
      ? "border-callout-warning/40 bg-callout-warning/15 text-callout-warning"
      : tone === "success"
        ? "border-callout-success/40 bg-callout-success/15 text-callout-success"
        : "border-accent/40 bg-accent/15 text-accent";
  return (
    <div
      className={`flex items-center justify-center gap-1 border-b px-3 py-1 text-[10px] ${classes}`}
    >
      {children}
    </div>
  );
}
