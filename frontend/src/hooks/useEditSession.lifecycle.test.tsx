import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { HEARTBEAT_MS, useEditSession } from "./useEditSession";
import type { EditSession } from "~/api/editsession";

// Mock the edit-session API so the lifecycle (acquire on mount, heartbeat on the interval,
// release on unmount, takeover) is provable with spies — no browser, no backend.
vi.mock("~/api/editsession", () => ({
  editSessionApi: {
    get: vi.fn(),
    acquire: vi.fn(),
    heartbeat: vi.fn(),
    release: vi.fn(),
    takeover: vi.fn(),
  },
}));
import { editSessionApi } from "~/api/editsession";

const mockApi = editSessionApi as unknown as Record<keyof typeof editSessionApi, ReturnType<typeof vi.fn>>;

const liveMine = (): EditSession => ({
  page_id: "p1",
  workspace_id: "w1",
  holder: "me",
  acquired_at: "2026-01-01T00:00:00Z",
  last_heartbeat: "2026-01-01T00:00:00Z",
  live: true,
});

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.setItem("docs_member_id", "me");
  mockApi.get.mockResolvedValue(null);
  mockApi.acquire.mockResolvedValue(liveMine());
  mockApi.release.mockResolvedValue({ ok: true });
  mockApi.takeover.mockResolvedValue(liveMine());
  mockApi.heartbeat.mockResolvedValue(liveMine());
});
afterEach(() => {
  localStorage.clear();
});

describe("useEditSession lifecycle", () => {
  it("autoAcquire=true → acquires on mount, releases on unmount", async () => {
    const { unmount } = renderHook(() => useEditSession("s1", "p1", { autoAcquire: true }), {
      wrapper,
    });
    await waitFor(() => expect(mockApi.acquire).toHaveBeenCalledTimes(1));
    expect(mockApi.release).not.toHaveBeenCalled();

    unmount();
    await waitFor(() => expect(mockApi.release).toHaveBeenCalledTimes(1));
  });

  it("autoAcquire=false (default) → never acquires (an observer/read-only viewer)", async () => {
    renderHook(() => useEditSession("s1", "p1"), { wrapper });
    await new Promise((r) => setTimeout(r, 20));
    expect(mockApi.acquire).not.toHaveBeenCalled();
  });

  it("takeover.mutate() calls the takeover endpoint", async () => {
    const { result } = renderHook(() => useEditSession("s1", "p1"), { wrapper });
    result.current.takeover.mutate();
    await waitFor(() => expect(mockApi.takeover).toHaveBeenCalledTimes(1));
  });

  it("heartbeats on the interval while I hold the slot", async () => {
    vi.useFakeTimers();
    try {
      mockApi.get.mockResolvedValue(liveMine()); // the query sees me holding a live session
      const { result } = renderHook(() => useEditSession("s1", "p1"), { wrapper });

      // Flush the initial query so heldByMe becomes true and the heartbeat interval starts.
      await vi.advanceTimersByTimeAsync(5);
      expect(result.current.heldByMe).toBe(true);
      expect(mockApi.heartbeat).not.toHaveBeenCalled();

      // One interval elapses → exactly one heartbeat.
      await vi.advanceTimersByTimeAsync(HEARTBEAT_MS);
      expect(mockApi.heartbeat).toHaveBeenCalledTimes(1);

      // A second interval → a second heartbeat (it keeps the slot alive while open).
      await vi.advanceTimersByTimeAsync(HEARTBEAT_MS);
      expect(mockApi.heartbeat).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does NOT heartbeat when the slot is held by someone else", async () => {
    vi.useFakeTimers();
    try {
      mockApi.get.mockResolvedValue({ ...liveMine(), holder: "alice" });
      const { result } = renderHook(() => useEditSession("s1", "p1"), { wrapper });
      await vi.advanceTimersByTimeAsync(5);
      expect(result.current.heldByOther).toBe(true);
      await vi.advanceTimersByTimeAsync(HEARTBEAT_MS * 3);
      expect(mockApi.heartbeat).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});
