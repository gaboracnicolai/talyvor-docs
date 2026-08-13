import { beforeEach, describe, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { PageVersion } from "~/api/types";

// THE DIFF VIEW COULD NOT SHOW A RENAME, AND A RENAME IS THE ONE EDIT THIS REPO ADDED A VERSION
// ROW TO PRESERVE.
//
// A `page_versions` row has exactly TWO content-bearing columns, `title` and `content`
// (`0002_pages.sql`), and `RestoreVersion` writes BOTH of them back onto the live page — so a
// version IS the pair, by this repository's own definition of one. `versioning_title_only_save_
// test.go` then made a title-only save append a snapshot ("A RENAME IS A SAVE, AND UNTIL THIS
// FILE IT OPENED NO RESTORE POINT"), and its green assertions pin the exact production state:
// after a body save and a rename there are two versions, the newest carrying `Title: "Renamed"`
// and `Content: {"body":"1"}` — the SAME content as the one before it.
//
// `DiffVersions` returns both snapshots WHOLE (`{"from": fromV, "to": toV}`, handler.go:351) and
// `PageVersion` carries `title` with no omitempty, so the rename is on the wire and typed at the
// boundary. The SPA dropped it at the RENDER: `diffLines` diffed `prettyContent(from.content)`
// against `prettyContent(to.content)` and nothing else.
//
// ⚠ MEASURED ON THE SHIPPED COMPONENT BEFORE ANY CHANGE, by rendering it and reading the diff
// panel's textContent — not by reading the JSX — with the fixture the Go test proves production
// writes (v1 "Original" / v2 "Renamed", identical content):
//
//     Diff v1 → v2  {    "body": "1"  }
//
// Every line `same`. ZERO added, ZERO removed, and NEITHER title anywhere in the panel. The
// screen's answer for a revision that renamed the document was that the revision changed
// nothing — a positive claim, not a silence, because the panel renders the unchanged body
// beneath the heading exactly as it does for a genuinely identical pair.
//
// ⚠ WHY NO TEST COULD SEE IT: `VersionHistory.test.tsx` covers `lineDiff` — the pure LCS core —
// and only that. It is correct and stays. Five cases over a function that was never wrong,
// while the composition that decides WHAT TEXT IS DIFFED had no test at all. This file renders
// the component instead.
//
// ⚠ THE DISPLAY CHOICE, STATED RATHER THAN INHERITED: the title is diffed as a `title: …` line
// at the head of each snapshot, so a rename reads as one removed and one added line in the
// panel's existing +/- vocabulary and needs no second mechanism. The alternative — a separate
// "renamed from X to Y" strip above the diff — was rejected because it puts one half of a
// version behind a different widget, which is how the two halves drifted apart in the first
// place. Asserted BY MARKING (`- ` / `+ ` on the row) rather than by the literal prefix, so the
// wording can change without the guard going quiet about the property.
//
// Controls: scripts/w31-versiondiff-rename-controls-8c31.py — 7/7 as predicted, including the
// vacuity floor (C4: the defect restored with THIS FILE DELETED leaves the whole frontend suite
// green, which is the state main was in at 7bfa1cf).

const v = (version: number, title: string, content: string): PageVersion => ({
  id: `ver-${version}`,
  page_id: "pg-1",
  workspace_id: "ws-1",
  version,
  title,
  content,
  created_by: "mem-1",
  created_at: "2026-08-13T10:00:00Z",
});

// Mutable so each test installs its own pair; the mock is hoisted and must not close over a
// per-test binding that does not exist yet.
let versions: PageVersion[] = [];

vi.mock("~/api/pages", () => ({
  pagesApi: {
    // GetVersions is ORDER BY version DESC — the list renders newest first, and the component
    // picks min/max itself, so the fixture keeps the real order rather than a convenient one.
    versions: vi.fn(async () => versions),
    diffVersions: vi.fn(async (_s: string, _p: string, from: number, to: number) => ({
      from: versions.find((x) => x.version === from)!,
      to: versions.find((x) => x.version === to)!,
    })),
    restore: vi.fn(async () => ({})),
  },
}));

import { VersionHistory } from "./VersionHistory";

// renderDiffRows mounts the real component, selects two versions the way a reader does (clicking
// both rows), and returns the diff panel's rendered lines with their +/- marking intact.
//
// ⚠ THE PANEL IS FOUND STRUCTURALLY (the one <pre> this component renders) RATHER THAN BY ITS
// HEADING TEXT. Anchoring on "Diff v1 → v2" would make every one of these assertions go silent
// the day somebody rewords a label — a guard that reds on prose teaches its readers it is about
// wording, and the cheapest way to quiet it is to change the sentence.
async function renderDiffRows(a: number, b: number): Promise<string[]> {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { container } = render(
    <QueryClientProvider client={qc}>
      <VersionHistory spaceID="sp-1" pageID="pg-1" />
    </QueryClientProvider>,
  );
  const u = userEvent.setup();
  await u.click(await screen.findByText(`v${a}`));
  await u.click(await screen.findByText(`v${b}`));
  // Selecting two versions renders the panel; the diff itself arrives on a second tick, so wait
  // for rendered ROWS rather than for the panel — the panel exists while it still says "Loading".
  let pre!: HTMLPreElement;
  await waitFor(() => {
    const el = container.querySelector("pre");
    if (!el || el.querySelectorAll("div").length === 0) {
      throw new Error("diff rows not rendered yet");
    }
    pre = el as HTMLPreElement;
  });
  return Array.from(pre.querySelectorAll("div")).map((d) => d.textContent ?? "");
}

const removed = (rows: string[]) => rows.filter((r) => r.startsWith("- "));
const added = (rows: string[]) => rows.filter((r) => r.startsWith("+ "));

beforeEach(() => {
  versions = [];
});

describe("the version diff over a title-only save", () => {
  // [RENAME-VISIBLE] — the exact pair TestTitleOnlySave_AppendsARestorePoint_RealPG proves
  // production writes: identical content, different title.
  it("shows the old title removed and the new one added", async () => {
    versions = [v(2, "Renamed", `{"body":"1"}`), v(1, "Original", `{"body":"1"}`)];
    const rows = await renderDiffRows(1, 2);

    if (!removed(rows).some((r) => r.includes("Original"))) {
      throw new Error(
        `[RENAME-VISIBLE] the diff of v1→v2 does not show the OLD title as removed. A rename is ` +
          `the only change between these two versions and the panel reports none, so the screen ` +
          `says a revision that renamed the document changed nothing. Rendered rows: ` +
          JSON.stringify(rows),
      );
    }
    if (!added(rows).some((r) => r.includes("Renamed"))) {
      throw new Error(
        `[RENAME-VISIBLE] the diff of v1→v2 does not show the NEW title as added. Rendered rows: ` +
          JSON.stringify(rows),
      );
    }
  });

  // [NO-PHANTOM-RENAME] — MUST STAY GREEN. Printing the title unconditionally as a change would
  // satisfy [RENAME-VISIBLE] while telling every reader of every ordinary body edit that the
  // document was also renamed. The title line has to participate in the diff, not bypass it.
  it("reports no title change when only the body changed", async () => {
    versions = [v(2, "Same name", `{"body":"2"}`), v(1, "Same name", `{"body":"1"}`)];
    const rows = await renderDiffRows(1, 2);

    const changed = [...removed(rows), ...added(rows)];
    if (changed.some((r) => r.includes("Same name"))) {
      throw new Error(
        `[NO-PHANTOM-RENAME] a body-only edit is reported as a title change: the title is being ` +
          `printed as added/removed rather than diffed. Rendered rows: ` + JSON.stringify(rows),
      );
    }
    // ANCHOR: the fixture must actually differ, or the assertion above passes vacuously.
    if (changed.length === 0) {
      throw new Error(
        `[NO-PHANTOM-RENAME] ANCHOR: the body-only fixture produced no diff at all, so the ` +
          `assertion above proves nothing. Rendered rows: ` + JSON.stringify(rows),
      );
    }
  });

  // [CONTENT-STILL-DIFFED] — a "fix" that diffs the title INSTEAD of the content, or that
  // reformats the snapshot so the body stops being compared, is a regression of the feature the
  // panel already had. Both halves of a version, or it is not a version diff.
  it("still diffs the body when both title and body changed", async () => {
    versions = [v(2, "Renamed", `{"body":"2"}`), v(1, "Original", `{"body":"1"}`)];
    const rows = await renderDiffRows(1, 2);

    if (!removed(rows).some((r) => r.includes(`"1"`)) || !added(rows).some((r) => r.includes(`"2"`))) {
      throw new Error(
        `[CONTENT-STILL-DIFFED] the body change is missing from the diff. Rendered rows: ` +
          JSON.stringify(rows),
      );
    }
    if (!removed(rows).some((r) => r.includes("Original")) || !added(rows).some((r) => r.includes("Renamed"))) {
      throw new Error(
        `[CONTENT-STILL-DIFFED] the title change is missing from a diff that carries the body ` +
          `change. Rendered rows: ` + JSON.stringify(rows),
      );
    }
  });
});
