import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { EditingBanner } from "./EditingBanner";
import type { SessionFlags } from "~/hooks/useEditSession";

const flags = (over: Partial<SessionFlags>): SessionFlags => ({
  holder: null,
  live: false,
  heldByMe: false,
  heldByOther: false,
  ...over,
});

describe("EditingBanner", () => {
  it("renders nothing when no one else holds the slot", () => {
    const { container } = render(
      <EditingBanner flags={flags({ heldByMe: true, live: true })} onTakeover={() => {}} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows '<holder> is editing' when someone else holds a live session", () => {
    render(
      <EditingBanner
        flags={flags({ heldByOther: true, live: true, holder: "mbr_alice" })}
        onTakeover={() => {}}
      />,
    );
    expect(screen.getByText(/is editing this page/i)).toBeInTheDocument();
    expect(screen.getByText(/mbr_alice/)).toBeInTheDocument();
  });

  it("prefers a resolved display name over the raw member id", () => {
    render(
      <EditingBanner
        flags={flags({ heldByOther: true, live: true, holder: "mbr_alice" })}
        holderName="Alice"
        onTakeover={() => {}}
      />,
    );
    expect(screen.getByText(/Alice is editing/)).toBeInTheDocument();
  });

  it("fires onTakeover when the button is clicked", async () => {
    const onTakeover = vi.fn();
    render(
      <EditingBanner
        flags={flags({ heldByOther: true, live: true, holder: "mbr_alice" })}
        onTakeover={onTakeover}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: /take over/i }));
    expect(onTakeover).toHaveBeenCalledOnce();
  });
});
