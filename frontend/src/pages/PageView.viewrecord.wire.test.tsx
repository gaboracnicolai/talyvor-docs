import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

// THE SCREEN OPENED EVERY DOCUMENT WITH A POST THE SERVER HAS NEVER ACCEPTED, AND THE
// `.catch(() => undefined)` NEXT TO IT GUARANTEED NOBODY WOULD EVER SEE IT FAIL.
//
// `PageView.tsx` carried two view-recording effects against the SAME route
// (`POST /v1/spaces/{s}/pages/{p}/view`):
//
//   1. on mount — `pagesApi.recordView(space.id, page.id)`, under the comment
//      "Record a view exactly once per page load"
//   2. on unmount + beforeunload — `analyticsApi.recordView(..., { duration_sec })`
//
// (1) SENDS NO BODY. `api/client.ts:146` only sets `init.body` when the caller passed one, and
// `pagesApi.recordView` (api/pages.ts) passes `{ method: "POST" }` and nothing else. The
// handler on the other end is `analytics/handler.go RecordView`, whose FIRST statement is
// `if err := json.NewDecoder(r.Body).Decode(&in); err != nil { 400 "bad json" }` — an empty
// body decodes to io.EOF. So the request 400s before it reaches any store call: no
// `page_views` row, no `pages.view_count` bump, no `last_viewed_at`. The comment above it
// describes a thing that has never happened.
//
// ⚠ AND IT IS DEAD TWICE OVER, WHICH IS WHY "make the handler tolerate an empty body" IS THE
// WRONG FIX. Even decoded, `in.Duration` would be 0 and `analytics/store.go:205`
// (`if view.Duration < minDuration { return nil }`, minDuration = 3) drops it — silently, with
// a 200 `{"ok":true}`. Tolerating the empty body would convert a visible 400 into a silent
// discard. The honest fix is to stop sending a request that cannot record anything; the
// duration-bearing flush in (2) is the one that does the work. `internal/analytics/
// recordview_emptybody_realpg_test.go` pins BOTH halves of that server contract on real
// Postgres, so the threshold this guard asserts is measured rather than assumed.
//
// ⚠ WHY THIS TEST STUBS `fetch` RATHER THAN MOCKING THE API MODULES. Every other PageView
// test mocks `~/api/pages` and `~/api/analytics` wholesale, so the defect lives in exactly the
// layer they replace: the question is not "was recordView called" (it was, twice) but "is what
// went ON THE WIRE something the server accepts". Only a fetch-level observation can answer
// that, and control C3 below shows the module-mocking tests cannot: with this file deleted the
// whole frontend suite stays green against the unfixed screen.

vi.mock("~/components/editor/Editor", () => ({ Editor: () => <div data-testid="editor" /> }));
vi.mock("~/components/ApprovalPanel", () => ({ ApprovalPanel: () => null }));
vi.mock("~/components/CommentsPanel", () => ({ CommentsPanel: () => null }));
vi.mock("~/components/CommentStatsBar", () => ({ CommentStatsBar: () => null }));
vi.mock("~/components/SharePanel", () => ({ SharePanel: () => null }));
vi.mock("~/components/ExportMenu", () => ({ ExportMenu: () => null }));
vi.mock("~/components/editor/PresenceBar", () => ({ PresenceBar: () => null }));
vi.mock("~/components/FreshnessBadge", () => ({ FreshnessBadge: () => null }));
vi.mock("~/components/FreshnessPanel", () => ({ FreshnessPanel: () => null }));
vi.mock("~/components/DocStatusBadge", () => ({ DocStatusBadge: () => null }));
vi.mock("~/components/editor/IssueSearchDialog", () => ({ IssueSearchDialog: () => null }));
vi.mock("~/components/editor/blocks/IssueEmbed", () => ({ IssueEmbed: () => null }));
vi.mock("~/components/VersionHistory", () => ({ VersionHistory: () => null }));
vi.mock("~/components/LinkedIssuesSection", () => ({ LinkedIssuesSection: () => null }));
vi.mock("~/components/changelog/ChangelogView", () => ({ ChangelogView: () => null }));

// The page/lock/session/freshness/link surfaces are mocked so the ONLY traffic reaching the
// stubbed fetch is the view-recording under test. `~/api/pages` and `~/api/analytics` are
// deliberately NOT mocked — they are the code whose wire shape is the subject.
vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({
    data: {
      id: "p1",
      workspace_id: "w1",
      space_id: "s1",
      title: "Doc",
      content: "{}",
      content_text: "hi there",
      doc_status: "draft",
      icon: "📄",
      view_count: 0,
      ai_cost_usd: 0,
      own_ai_cost_usd: 0,
      total_ai_cost_usd: 0,
      created_by: "alice",
      updated_by: "alice",
      updated_at: "2026-01-01T00:00:00Z",
      parent_id: null,
      page_type: "document",
      last_verified_at: null,
    },
    isLoading: false,
  }),
  useUpdatePage: () => ({ mutate: vi.fn(), isPending: false }),
}));
vi.mock("~/hooks/usePageLock", () => ({
  usePageLock: () => ({
    state: undefined,
    lockedByMe: false,
    lock: { mutate: vi.fn(), isPending: false },
    unlock: { mutate: vi.fn(), isPending: false },
  }),
}));
vi.mock("~/api/editsession", () => ({
  editSessionApi: {
    get: vi.fn().mockResolvedValue(null),
    acquire: vi.fn(),
    heartbeat: vi.fn(),
    release: vi.fn(),
    takeover: vi.fn(),
  },
}));
vi.mock("~/api/freshness", () => ({ freshnessApi: { forPage: vi.fn().mockResolvedValue(null) } }));
vi.mock("~/api/links", () => ({
  linksApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn(), remove: vi.fn() },
}));
vi.mock("~/api/templates", () => ({
  templatesApi: { list: vi.fn().mockResolvedValue([]), fromPage: vi.fn(), use: vi.fn(), delete: vi.fn() },
}));

import { PageViewPage } from "./PageView";

// minDuration in internal/analytics/store.go:22. A view POST carrying less than this is
// accepted with 200 {"ok":true} and then DISCARDED — so "the request has a body" is not the
// property worth guarding; "the request can actually record a view" is. Control C2 is what
// proves this line is load-bearing rather than decoration.
const SERVER_MIN_DURATION_SEC = 3;

interface WireCall {
  method: string;
  path: string;
  body?: string;
}

const calls: WireCall[] = [];
let clock = 1_700_000_000_000;

const space = { id: "s1", name: "Space", workspace_id: "w1" } as never;

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <PageViewPage space={space} pageID="p1" />
    </QueryClientProvider> as ReactNode,
  );
}

beforeEach(() => {
  calls.length = 0;
  clock = 1_700_000_000_000;
  vi.spyOn(Date, "now").mockImplementation(() => clock);
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      calls.push({
        method: (init?.method ?? "GET").toUpperCase(),
        path: String(url),
        body: init?.body === undefined || init?.body === null ? undefined : String(init.body),
      });
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "OK",
        json: () => Promise.resolve({ ok: true }),
      } as unknown as Response);
    }),
  );
  localStorage.setItem("docs_member_id", "me");
  localStorage.setItem("docs_member_name", "Me");
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const viewPosts = () =>
  calls.filter((c) => c.method === "POST" && /\/pages\/[^/]+\/view(?:\?|$)/.test(c.path));

describe("PageView only sends view records the server can actually record", () => {
  it("opening and leaving a document sends at least one view POST", () => {
    const { unmount } = renderPage();
    clock += 10_000;
    unmount();

    // NON-VACUITY ANCHOR. Without this the "every view POST is well-formed" assertion below
    // passes for free the moment the screen stops recording views at all — which is the exact
    // failure a careless "fix" (deleting both effects) would introduce. Control C4 restores
    // that state and this line is what reddens.
    expect(
      viewPosts().length,
      "[VIEW-RECORDED-AT-ALL] PageView recorded NO view at all for a 10s visit — the analytics flush is gone, and the " +
        "well-formedness assertion below would have passed vacuously.\n" +
        `wire calls seen: ${JSON.stringify(calls, null, 2)}`,
    ).toBeGreaterThan(0);
  });

  it("every view POST carries a body the server accepts and a duration it will not discard", () => {
    const { unmount } = renderPage();
    clock += 10_000;
    unmount();

    for (const call of viewPosts()) {
      // analytics/handler.go RecordView decodes the body FIRST and 400s on io.EOF, so a
      // bodyless POST here is a request that has never once been accepted.
      expect(
        call.body,
        `[VIEW-POST-HAS-BODY] POST ${call.path} was sent with NO request body. analytics/handler.go RecordView ` +
          "decodes r.Body before anything else and answers 400 \"bad json\" on an empty one, " +
          "so this request records nothing — no page_views row, no view_count bump — and the " +
          "caller's .catch(() => undefined) hides the 400.",
      ).toBeTypeOf("string");

      const parsed = JSON.parse(call.body as string) as { duration_sec?: unknown };
      expect(
        typeof parsed.duration_sec,
        `[VIEW-POST-HAS-BODY] POST ${call.path} sent a body with no numeric duration_sec: ${call.body}`,
      ).toBe("number");
      expect(
        parsed.duration_sec as number,
        `[VIEW-POST-ABOVE-MIN-DURATION] POST ${call.path} sent duration_sec=${String(parsed.duration_sec)}, below the ` +
          `server's minDuration of ${SERVER_MIN_DURATION_SEC}s (internal/analytics/store.go). ` +
          "The server answers 200 {\"ok\":true} and then drops it, so this view is recorded " +
          "nowhere while the client is told it succeeded.",
      ).toBeGreaterThanOrEqual(SERVER_MIN_DURATION_SEC);
    }
  });
});
