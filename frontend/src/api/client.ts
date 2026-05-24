// Thin fetch wrapper. Attaches the Authorization header from local
// storage (Phase 2 doesn't ship a login flow — Phase 3 will), throws
// a typed error on non-2xx, and decodes JSON responses.

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

interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
}

export async function apiRequest<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { body, headers, ...rest } = options;
  const token = localStorage.getItem("docs_api_key") ?? "";
  const memberId = localStorage.getItem("docs_member_id") ?? "";

  const init: RequestInit = {
    ...rest,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(memberId ? { "X-Member-Id": memberId } : {}),
      ...(headers ?? {}),
    },
  };
  if (body !== undefined) {
    init.body = typeof body === "string" ? body : JSON.stringify(body);
  }

  const res = await fetch(BASE + path, init);
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
  return (await res.json()) as T;
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
