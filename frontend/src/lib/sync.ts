// SyncManager flushes the offline write queue back to the server.
// One global instance lives in App.tsx — auto-syncs every 30s while
// online, on window focus, and on the browser's "online" event.
//
// We deliberately don't depend on Background Sync API support
// because Safari + Firefox don't ship it yet. Polling is fine for
// the per-user write volume Docs sees.

import {
  bumpRetries,
  getWriteQueue,
  removeFromQueue,
  setSyncStatus,
  type WriteQueueItem,
} from "./offlinedb";

const MAX_RETRIES = 3;
const BASE = (import.meta.env.VITE_API_URL as string | undefined) ?? "";

export type SyncListener = (state: {
  syncing: boolean;
  pending: number;
  lastSyncAt?: number;
  failed?: WriteQueueItem[];
}) => void;

export class SyncManager {
  private timer: ReturnType<typeof setInterval> | null = null;
  private listeners = new Set<SyncListener>();
  private inFlight = false;
  private failed: WriteQueueItem[] = [];

  subscribe(fn: SyncListener): () => void {
    this.listeners.add(fn);
    void this.emit();
    return () => this.listeners.delete(fn);
  }

  private async emit() {
    const queue = await safeQueue();
    const state = {
      syncing: this.inFlight,
      pending: queue.length,
      failed: this.failed.slice(),
    };
    this.listeners.forEach((l) => l(state));
  }

  // syncQueue drains the write_queue. Each item is replayed with
  // the original method/URL/body; success removes it; failure
  // bumps retries. Past MAX_RETRIES the item moves to the in-memory
  // failed list — we don't delete from IndexedDB so the user can
  // see them in settings and retry manually.
  async syncQueue(): Promise<void> {
    if (this.inFlight) return;
    if (typeof navigator !== "undefined" && navigator.onLine === false) return;
    this.inFlight = true;
    await this.emit();
    try {
      const queue = await safeQueue();
      for (const item of queue) {
        if (item.retries >= MAX_RETRIES) {
          if (!this.failed.find((f) => f.id === item.id)) this.failed.push(item);
          continue;
        }
        try {
          const res = await fetch(BASE + item.url, {
            method: item.method,
            headers: { "Content-Type": "application/json" },
            body: item.body,
          });
          if (res.ok || (res.status >= 400 && res.status < 500)) {
            // 4xx is terminal — the request will never succeed (bad
            // payload, deleted resource). Treat it like a successful
            // drain so we don't loop forever.
            if (item.id != null) await removeFromQueue(item.id);
          } else {
            await bumpRetries(item);
          }
        } catch {
          await bumpRetries(item);
        }
      }
      await setSyncStatus("last_sync", Date.now());
    } finally {
      this.inFlight = false;
      await this.emit();
    }
  }

  // startAutoSync wires the three "let's check now" triggers:
  // periodic interval, window focus, and the navigator.online event.
  startAutoSync(intervalMs = 30_000): void {
    if (typeof window === "undefined") return;
    this.stopAutoSync();
    this.timer = setInterval(() => {
      void this.syncQueue();
    }, intervalMs);
    window.addEventListener("focus", this.onFocus);
    window.addEventListener("online", this.onOnline);
  }

  stopAutoSync(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
    if (typeof window !== "undefined") {
      window.removeEventListener("focus", this.onFocus);
      window.removeEventListener("online", this.onOnline);
    }
  }

  // Arrow-bound so they can be added/removed without losing `this`.
  private onFocus = () => {
    void this.syncQueue();
  };
  private onOnline = () => {
    void this.syncQueue();
  };

  clearFailed(): void {
    this.failed = [];
    void this.emit();
  }
}

async function safeQueue(): Promise<WriteQueueItem[]> {
  try {
    return await getWriteQueue();
  } catch {
    return [];
  }
}

// Shared singleton — App.tsx subscribes on mount and triggers
// syncs on user-driven events.
export const syncManager = new SyncManager();
