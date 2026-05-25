// Talyvor Docs offline cache. Thin promise wrapper around the
// browser's IndexedDB so the React layer never touches the
// callback-style API directly.
//
// The store is per-browser — clearing the cache is offered in the
// settings UI; nothing here syncs across devices.

import type { Page, Space } from "~/api/types";

const DB_NAME = "talyvor-docs";
const DB_VERSION = 1;

export interface WriteQueueItem {
  id?: number;
  method: "PATCH" | "POST" | "PUT" | "DELETE";
  url: string;
  body: string;
  timestamp: number;
  retries: number;
  // Optional human-readable label the indicator can show ("Save
  // page Foo", "Delete comment"). Best-effort only.
  label?: string;
}

// openDB resolves to a singleton database. Re-opening is cheap but
// holding the handle avoids the per-call promise overhead.
let dbPromise: Promise<IDBDatabase> | null = null;
export function openDB(): Promise<IDBDatabase> {
  if (typeof indexedDB === "undefined") {
    return Promise.reject(new Error("IndexedDB unavailable"));
  }
  if (!dbPromise) {
    dbPromise = new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VERSION);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains("pages")) {
          const store = db.createObjectStore("pages", { keyPath: "id" });
          store.createIndex("spaceId", "space_id", { unique: false });
          store.createIndex("updatedAt", "updated_at", { unique: false });
        }
        if (!db.objectStoreNames.contains("spaces")) {
          db.createObjectStore("spaces", { keyPath: "id" });
        }
        if (!db.objectStoreNames.contains("write_queue")) {
          db.createObjectStore("write_queue", {
            keyPath: "id",
            autoIncrement: true,
          });
        }
        if (!db.objectStoreNames.contains("sync_status")) {
          db.createObjectStore("sync_status", { keyPath: "key" });
        }
      };
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    });
  }
  return dbPromise;
}

// withStore is the tiny helper every CRUD function uses: open the
// DB, get a typed object store in the right mode, run the callback,
// and resolve when the transaction completes.
async function withStore<T>(
  name: string,
  mode: IDBTransactionMode,
  fn: (s: IDBObjectStore) => IDBRequest | undefined,
): Promise<T> {
  const db = await openDB();
  return new Promise<T>((resolve, reject) => {
    const tx = db.transaction(name, mode);
    const store = tx.objectStore(name);
    const req = fn(store);
    tx.oncomplete = () => {
      // For mutations we return undefined; for reads the caller's
      // request `result` is what matters.
      if (req && "result" in req) resolve(req.result as T);
      else resolve(undefined as unknown as T);
    };
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error);
  });
}

// ─── Pages ──────────────────────────────────────────

export function cachePage(page: Page): Promise<void> {
  return withStore("pages", "readwrite", (s) => s.put(page));
}

export function getCachedPage(id: string): Promise<Page | null> {
  return withStore<Page | undefined>("pages", "readonly", (s) => s.get(id)).then(
    (v) => v ?? null,
  );
}

export async function getCachedPages(spaceID: string): Promise<Page[]> {
  const db = await openDB();
  return new Promise<Page[]>((resolve, reject) => {
    const tx = db.transaction("pages", "readonly");
    const store = tx.objectStore("pages");
    const index = store.index("spaceId");
    const req = index.getAll(spaceID);
    req.onsuccess = () => resolve(req.result as Page[]);
    req.onerror = () => reject(req.error);
  });
}

// ─── Spaces ─────────────────────────────────────────

export function cacheSpace(space: Space): Promise<void> {
  return withStore("spaces", "readwrite", (s) => s.put(space));
}

export async function getCachedSpaces(): Promise<Space[]> {
  const db = await openDB();
  return new Promise<Space[]>((resolve, reject) => {
    const tx = db.transaction("spaces", "readonly");
    const req = tx.objectStore("spaces").getAll();
    req.onsuccess = () => resolve(req.result as Space[]);
    req.onerror = () => reject(req.error);
  });
}

// ─── Write queue ────────────────────────────────────

export function queueWrite(item: Omit<WriteQueueItem, "id" | "timestamp" | "retries">): Promise<void> {
  return withStore("write_queue", "readwrite", (s) =>
    s.add({
      ...item,
      timestamp: Date.now(),
      retries: 0,
    }),
  );
}

export async function getWriteQueue(): Promise<WriteQueueItem[]> {
  const db = await openDB();
  return new Promise<WriteQueueItem[]>((resolve, reject) => {
    const tx = db.transaction("write_queue", "readonly");
    const req = tx.objectStore("write_queue").getAll();
    req.onsuccess = () => resolve(req.result as WriteQueueItem[]);
    req.onerror = () => reject(req.error);
  });
}

export function removeFromQueue(id: number): Promise<void> {
  return withStore("write_queue", "readwrite", (s) => s.delete(id));
}

export function bumpRetries(item: WriteQueueItem): Promise<void> {
  return withStore("write_queue", "readwrite", (s) =>
    s.put({ ...item, retries: item.retries + 1 }),
  );
}

// ─── Sync status ────────────────────────────────────

export function setSyncStatus(key: string, value: unknown): Promise<void> {
  return withStore("sync_status", "readwrite", (s) => s.put({ key, value }));
}

export async function getSyncStatus<T = unknown>(key: string): Promise<T | null> {
  const out = await withStore<{ value: T } | undefined>(
    "sync_status",
    "readonly",
    (s) => s.get(key),
  );
  return out?.value ?? null;
}

// ─── Cache maintenance ──────────────────────────────

export async function clearCache(): Promise<void> {
  const db = await openDB();
  return new Promise<void>((resolve, reject) => {
    const tx = db.transaction(["pages", "spaces", "sync_status"], "readwrite");
    tx.objectStore("pages").clear();
    tx.objectStore("spaces").clear();
    tx.objectStore("sync_status").clear();
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

// estimateCache returns the storage-API estimate when available.
// Browsers without the API return { quota: 0, usage: 0 }; callers
// surface "—" in the UI.
export async function estimateCache(): Promise<{ usage: number; quota: number }> {
  if (
    typeof navigator !== "undefined" &&
    navigator.storage &&
    "estimate" in navigator.storage
  ) {
    const { usage = 0, quota = 0 } = await navigator.storage.estimate();
    return { usage, quota };
  }
  return { usage: 0, quota: 0 };
}
