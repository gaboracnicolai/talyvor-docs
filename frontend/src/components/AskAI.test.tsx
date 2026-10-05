import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { queueWrite } from "~/lib/offlinedb";
import { AskAI, ASK_TIMEOUT_MS } from "./AskAI";

// Every click on Ask ends in an answer or a sentence saying why not. Before B28.273 a network
// failure was queued as an offline write and resolved empty, so the panel showed nothing at all.

vi.mock("~/lib/offlinedb", () => ({
  cachePage: vi.fn(),
  cacheSpace: vi.fn(),
  getCachedPage: vi.fn(),
  getCachedPages: vi.fn(),
  getCachedSpaces: vi.fn(),
  queueWrite: vi.fn().mockResolvedValue(undefined),
}));

const fetchMock = vi.fn();

function openAndAsk(question: string) {
  render(<AskAI workspaceId="ws1" />);
  fireEvent.click(screen.getByTitle(/Ask AI/));
  fireEvent.change(screen.getByPlaceholderText(/Ask anything/), { target: { value: question } });
  fireEvent.click(screen.getByRole("button", { name: "Ask" }));
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.mocked(queueWrite).mockClear();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("AskAI", () => {
  it("shows the answer and the page it came from", async () => {
    fetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          answer: "The launch is on 12 March.",
          sources: [{ title: "Launch memo", url: "/spaces/s1/pages/p1" }],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    openAndAsk("When is the launch?");
    expect(await screen.findByText("The launch is on 12 March.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Launch memo" })).toHaveAttribute("href", "/spaces/s1/pages/p1");
  });

  it("shows an error when the request never reaches the server, and does not queue it", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    openAndAsk("When is the launch?");
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't reach Docs");
    expect(queueWrite).not.toHaveBeenCalled();
  });

  it("stops waiting and says so when no answer arrives in time", async () => {
    vi.useFakeTimers();
    fetchMock.mockImplementation(
      (_url: string, init: RequestInit) =>
        new Promise((_resolve, reject) => {
          init.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
        }),
    );
    openAndAsk("When is the launch?");
    expect(screen.getByText("Thinking…")).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ASK_TIMEOUT_MS);
    });
    expect(screen.getByRole("alert")).toHaveTextContent(`no answer within ${ASK_TIMEOUT_MS / 1000} seconds`);
    expect(screen.queryByText("Thinking…")).not.toBeInTheDocument();
  });

  it("tells you to type a question when Ask is clicked with an empty box", () => {
    openAndAsk("   ");
    expect(screen.getByRole("alert")).toHaveTextContent("Type a question first.");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shows the server's error when Ask is refused", async () => {
    fetchMock.mockResolvedValue(
      new Response(JSON.stringify({ error: "AI unavailable. Check Lens configuration.", code: "AI_FAILED" }), {
        status: 502,
        headers: { "Content-Type": "application/json" },
      }),
    );
    openAndAsk("When is the launch?");
    expect(await screen.findByRole("alert")).toHaveTextContent("Ask didn't answer: AI unavailable.");
  });
});
