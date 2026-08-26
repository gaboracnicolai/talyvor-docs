import { describe, expect, it } from "vitest";
import { revisionCost } from "./VersionHistory";

// COST PER REVISION IN VERSION HISTORY — the bullet talyvor.higgsfield.app/products/docs sells
// under "The detail". The server side is migration 0021 plus page.Store.GetVersions; this is the
// half a reader actually sees, and it is where a true number becomes a false sentence.
//
// ⚠ THE DEFECT THIS GUARDS IS ROUNDING A REAL CHARGE TO "$0.00". Every other money render in this
// SPA is toFixed(2) because it prices whole documents and linked Track issues. One revision
// routinely costs a fraction of a cent, and at two decimals a charge that exists prints as the
// same six characters as no charge at all — the one rounding error a reader has no way to detect.
describe("revisionCost", () => {
  it("never renders a non-zero cost as $0.00", () => {
    for (const usd of [0.009, 0.001, 0.0004, 0.0001, 0.00004, 0.000001]) {
      const got = revisionCost(usd);
      expect(got, `${usd} rendered as ${got}`).not.toBe("$0.00");
      expect(got, `${usd} rendered as ${got}`).not.toMatch(/^\$0\.0+$/);
      expect(got, `${usd} rendered as ${got}`).not.toBe("—");
    }
  });

  it("shows four decimals below a cent, two at or above it", () => {
    expect(revisionCost(0.004)).toBe("$0.0040");
    expect(revisionCost(0.0099)).toBe("$0.0099");
    expect(revisionCost(0.01)).toBe("$0.01");
    expect(revisionCost(0.0345)).toBe("$0.03");
    expect(revisionCost(1.5)).toBe("$1.50");
  });

  it("reports amounts below the four-decimal floor as a bound, not as a rounded value", () => {
    expect(revisionCost(0.00009)).toBe("<$0.0001");
    expect(revisionCost(0.000001)).toBe("<$0.0001");
  });

  // ⚠ ZERO IS NOT "$0.00", AND THE DIFFERENCE IS THE WHOLE HONESTY OF THE FEATURE. Zero means no
  // priced spend is ATTRIBUTED to this revision. Spend bound after the newest save belongs to a
  // revision that does not exist yet, and every binding written before migration 0021 carries no
  // revision at all; both are real money the server reports separately and neither can reach this
  // row. "$0.00" would state that the revision cost nothing, which the server never claimed.
  it("renders zero and absent as an em dash rather than a price", () => {
    expect(revisionCost(0)).toBe("—");
    expect(revisionCost(-0)).toBe("—");
    expect(revisionCost(NaN)).toBe("—");
    expect(revisionCost(undefined as unknown as number)).toBe("—");
  });
});
