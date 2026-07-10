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
  if (path.match(/\/v1\/spaces\/[^/]+\/pages\/[^/]+$/)) return true;
  if (path.endsWith("/v1/spaces")) return true;
  return false;
}

// readCached looks up a cached response for a GET. Returns null
// when no cached entry exists.
async function readCached<T>(path: string): Promise<T | null> {
  // /v1/spaces/:spaceID/pages/:pageID → pageID is the trailing
  // segment.
  const pageMatch = path.match(/\/v1\/spaces\/([^/]+)\/pages\/([^/?]+)/);
  if (pageMatch) {
    const page = await getCachedPage(pageMatch[2]);
    return (page as unknown as T) ?? null;
  }
  // /v1/spaces (list) → return every cached space.
  if (path.endsWith("/v1/spaces")) {
    const spaces = await getCachedSpaces();
    return (spaces as unknown as T) ?? null;
  }
  // /v1/spaces/:spaceID/pages → cached pages for that space.
  const listMatch = path.match(/\/v1\/spaces\/([^/]+)\/pages\b/);
  if (listMatch && !listMatch[0].includes("/pages/")) {
    const pages = await getCachedPages(listMatch[1]);
    return (pages as unknown as T) ?? null;
  }
  return null;
}

// writeCache persists a successful response for the cacheable
// reads. Errors are swallowed — caching is best-effort, never
// blocking the API consumer.
async function writeCache(path: string, data: unknown): Promise<void> {
  try {
    const pageMatch = path.match(/\/v1\/spaces\/([^/]+)\/pages\/([^/?]+)/);
    if (pageMatch) {
      await cachePage(data as Page);
      return;
    }
    if (path.endsWith("/v1/spaces") && Array.isArray(data)) {
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
