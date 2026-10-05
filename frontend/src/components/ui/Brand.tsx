import logoUrl from "~/assets/talyvor-logo-dark-notag.svg";

// The brand files are copied from brand-v4/svg, never redrawn: the
// wordmark is drawn, so TALYVOR is never set as live text.

// DocsBrand is the mark and wordmark with the product named beside it
// as an eyebrow — the board's product-UI sidebar header, as in Track.
export function DocsBrand({ size = "md" }: { size?: "sm" | "md" }) {
  return (
    <div className="flex items-center gap-2.5">
      <img src={logoUrl} alt="Talyvor" className={size === "sm" ? "h-4 w-auto" : "h-5 w-auto"} />
      <span className="text-[12px] font-medium uppercase tracking-[0.22em] text-label">Docs</span>
    </div>
  );
}
