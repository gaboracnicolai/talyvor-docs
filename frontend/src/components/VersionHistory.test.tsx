import { describe, expect, it } from "vitest";
import { lineDiff } from "./VersionHistory";

// lineDiff is the pure LCS diff behind the version-compare view — genuinely headless-correct.
describe("lineDiff", () => {
  it("identical text → all same, nothing added/removed", () => {
    const d = lineDiff("a\nb\nc", "a\nb\nc");
    expect(d.every((l) => l.type === "same")).toBe(true);
    expect(d.map((l) => l.text)).toEqual(["a", "b", "c"]);
  });

  it("an inserted line is 'add', keeping surrounding lines as 'same'", () => {
    const d = lineDiff("a\nc", "a\nb\nc");
    expect(d).toEqual([
      { type: "same", text: "a" },
      { type: "add", text: "b" },
      { type: "same", text: "c" },
    ]);
  });

  it("a deleted line is 'remove'", () => {
    const d = lineDiff("a\nb\nc", "a\nc");
    expect(d).toEqual([
      { type: "same", text: "a" },
      { type: "remove", text: "b" },
      { type: "same", text: "c" },
    ]);
  });

  it("a changed line is remove-then-add (LCS keeps the common anchors)", () => {
    const d = lineDiff("a\nX\nc", "a\nY\nc");
    expect(d.filter((l) => l.type === "remove").map((l) => l.text)).toEqual(["X"]);
    expect(d.filter((l) => l.type === "add").map((l) => l.text)).toEqual(["Y"]);
    expect(d.filter((l) => l.type === "same").map((l) => l.text)).toEqual(["a", "c"]);
  });

  it("reconstructing 'to' from same+add lines round-trips", () => {
    const from = "one\ntwo\nthree";
    const to = "one\nthree\nfour";
    const rebuilt = lineDiff(from, to)
      .filter((l) => l.type !== "remove")
      .map((l) => l.text)
      .join("\n");
    expect(rebuilt).toBe(to);
  });
});
