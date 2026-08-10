// Offline-aware fetch wrapper.
//
// On success we cache GET responses in IndexedDB (pages + spaces
// only — those are the surfaces an offline reader needs). On
// network failure we fall back to the cached response for reads
// and queue writes for later replay via SyncManager.

import {
  cachePage,
  cacheSpace,
  getCachedPage,
  getCachedPages,
  getCachedSpaces,
  queueWrite,
} from "~/lib/offlinedb";
import type { Page, Space } from "./types";

const BASE = import.meta.env.VITE_API_URL ?? "";

export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
    public code?: string,
  ) {
    super(message);
    this.name = "APIError";
  }
}

export class OfflineQueuedError extends Error {
  constructor() {
    super("Queued for sync — currently offline");
    this.name = "OfflineQueuedError";
  }
}

interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
}

export function isOnline(): boolean {
  if (typeof navigator === "undefined") return true;
  return navigator.onLine !== false;
}

// shouldCache restricts the offline cache to the two GET surfaces an
// offline reader actually needs: page reads + space listings.
// Comments, analytics, freshness, etc. fall back to a live request
// and surface the offline error directly.
function shouldCache(method: string, path: string): boolean {
  if (method !== "GET") return false;
  // Same patterns readCached uses. This half was already anchored correctly; sharing the
  // definition is what stops the two halves from disagreeing again rather than by luck.
  return PAGE_DETAIL.test(path) || SPACE_LIST.test(path);
}

// readCached looks up a cached response for a GET. Returns null
// when no cached entry exists.
// THE THREE PATH SHAPES THIS CACHE KNOWS, DEFINED ONCE. Every reader and writer below is a
// join of this list, because the defect these anchors fix was TWO hand-written patterns for one
// question that differed by a single character.
//
// `readCached`'s page pattern used to be `/\/v1\/spaces\/([^/]+)\/pages\/([^/?]+)/` — anchored
// at neither end — while `shouldCache`'s was `/\/v1\/spaces\/[^/]+\/pages\/[^/]+$/`, anchored.
// So the cache STORED only pages, and SERVED a page for every sub-resource under one:
// `/pages/p1/comments`, `/comments/stats`, `/analytics`, `/permissions`, `/approval`,
// `/changelog/entries` and `/edit-session` each matched, pageID captured `p1`, the rest of the
// path was ignored, and the caller got a document where it asked for a list.
//
// ⚠ THAT FAILS AS A SUCCESS, WHICH IS WHY NOTHING SAW IT. This client's entire offline story
// hangs off fetch REJECTING — it is the branch that reads the cache and the branch that queues
// writes. The reject WAS taken here and the recovery then RESOLVED, so every caller saw a
// fulfilled promise carrying the wrong shape. No offline handler in this app can observe that.
//
// ⚠ AND NO TYPE COULD SEE IT EITHER: readCached returns `T | null` through
// `value as unknown as T`, so the cast is the whole safety story.
//
// `(?:\?|$)` rather than `$` so a query string still identifies the same resource.
const PAGE_DETAIL = /^\/v1\/spaces\/([^/]+)\/pages\/([^/?]+)(?:\?|$)/;
const PAGE_LIST = /^\/v1\/spaces\/([^/]+)\/pages(?:\?|$)/;
const SPACE_LIST = /^\/v1\/spaces(?:\?|$)/;

async function readCached<T>(path: string): Promise<T | null> {
  // Most specific first, for readability — and NOT because order is load-bearing. Control H4
  // swaps these two branches and NOTHING REDDENS: PAGE_LIST ends at `(?:\?|$)`, so it cannot
  // match a detail path however early it is tried. The anchors are what does the work here, and
  // that was measured rather than assumed — the first version of this comment claimed the
  // opposite and the control refused it.
  const pageMatch = path.match(PAGE_DETAIL);
  if (pageMatch) {
    const page = await getCachedPage(pageMatch[2]);
    return (page as unknown as T) ?? null;
  }
  const listMatch = path.match(PAGE_LIST);
  if (listMatch) {
    const pages = await getCachedPages(listMatch[1]);
    return (pages as unknown as T) ?? null;
  }
  if (SPACE_LIST.test(path)) {
    const spaces = await getCachedSpaces();
    return (spaces as unknown as T) ?? null;
  }
  // Anything else under /v1/spaces — every page sub-resource — was never cached, so there is
  // nothing to serve and the caller gets APIError("offline"). Reporting the outage is the
  // honest answer; answering with a different resource is not.
  return null;
}

// writeCache persists a successful response for the cacheable
// reads. Errors are swallowed — caching is best-effort, never
// blocking the API consumer.
async function writeCache(path: string, data: unknown): Promise<void> {
  try {
    // The SAME pattern readCached and shouldCache use. This regex was unanchored too; it was
    // unreachable for a sub-resource only because shouldCache gated it, so it was one edit away
    // from writing a comments array or an approval request into the `pages` object store.
    if (PAGE_DETAIL.test(path)) {
      await cachePage(data as Page);
      return;
    }
    if (SPACE_LIST.test(path) && Array.isArray(data)) {
      for (const sp of data as Space[]) await cacheSpace(sp);
      return;
    }
  } catch {
    // Storage quota / private mode — drop silently.
  }
}

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { body, headers, method = "GET", ...rest } = options;
  const token = localStorage.getItem("docs_api_key") ?? "";

  const init: RequestInit = {
    ...rest,
    method,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      // X-Member-Id removed: since SEC-4 the server derives identity from the gateway-verified
      // Authorization and ignores this client-sent header — dead weight, so stop sending it.
      ...(headers ?? {}),
    },
  };
  const encodedBody = body === undefined ? undefined : (typeof body === "string" ? body : JSON.stringify(body));
  if (encodedBody !== undefined) {
    init.body = encodedBody;
  }

  let res: Response;
  try {
    res = await fetch(BASE + path, init);
  } catch (err) {
    // Network failure — split between reads and writes.
    if (method === "GET") {
      const cached = await readCached<T>(path);
      if (cached !== null) return cached;
      throw new APIError("offline", 0, "OFFLINE");
    }
    // Writes go into the queue for SyncManager to replay later.
    // We resolve with `undefined as T` so optimistic UI works; the
    // OfflineQueuedError exists for callers that want to surface
    // the queued status (we don't throw it because most callers
    // would treat it as an error).
    await queueWrite({
      method: method as "POST" | "PATCH" | "PUT" | "DELETE",
      url: path,
      body: encodedBody ?? "",
      label: path,
    });
    return undefined as T;
    // Re-throw the original error for callers that wired a try/catch
    // around their mutation: not done — the queue path is the contract.
    void err;
  }
  if (!res.ok) {
    let msg = res.statusText;
    let code: string | undefined;
    try {
      const data = (await res.json()) as { error?: string; code?: string };
      msg = data.error ?? msg;
      code = data.code;
    } catch {
      // body wasn't JSON — fall back to status text
    }
    throw new APIError(msg, res.status, code);
  }
  if (res.status === 204) return undefined as T;
  const data = (await res.json()) as T;
  if (shouldCache(method, path)) {
    void writeCache(path, data);
  }
  return data;
}

// qs builds a query string from an object, dropping nullish values.
export function qs(params: Record<string, string | number | undefined>): string {
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    usp.set(k, String(v));
  }
  const s = usp.toString();
  return s ? `?${s}` : "";
}
