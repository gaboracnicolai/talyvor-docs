import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { paths } from "./paths";
import type { ResourceState } from "./guard";

// Terminal views for a guarded resource route. The NOT-FOUND copy is deliberately generic:
// it is shown for both a missing resource and one the caller may not see (guard.ts collapses
// 403→404), so it must never hint which. "You don't have access" here would re-create the
// existence oracle the guard exists to prevent.

export function NotFoundView() {
  return (
    <CenteredCard>
      <div className="text-2xl">🔍</div>
      <h1 className="text-sm font-semibold">Not found</h1>
      <p className="max-w-sm text-xs text-muted">
        This page doesn&apos;t exist, or isn&apos;t available to you.
      </p>
      <Link to={paths.home()} className="text-xs text-accent hover:underline">
        Back to home
      </Link>
    </CenteredCard>
  );
}

export function OfflineView() {
  return (
    <CenteredCard>
      <div className="text-2xl">📡</div>
      <h1 className="text-sm font-semibold">You&apos;re offline</h1>
      <p className="max-w-sm text-xs text-muted">
        This content isn&apos;t cached for offline use. Reconnect and try again.
      </p>
    </CenteredCard>
  );
}

export function LoadErrorView() {
  return (
    <CenteredCard>
      <div className="text-2xl">⚠️</div>
      <h1 className="text-sm font-semibold">Something went wrong</h1>
      <p className="max-w-sm text-xs text-muted">
        We couldn&apos;t load this right now. Please try again.
      </p>
      <Link to={paths.home()} className="text-xs text-accent hover:underline">
        Back to home
      </Link>
    </CenteredCard>
  );
}

export function LoadingView() {
  return <div className="p-8 text-sm text-muted">Loading…</div>;
}

// Guard renders the terminal view for a non-ready state, or its children when ready. Keeping
// the state→view mapping in one place means every resource route degrades identically.
export function Guard({ state, children }: { state: ResourceState; children: ReactNode }) {
  switch (state) {
    case "loading":
      return <LoadingView />;
    case "notfound":
      return <NotFoundView />;
    case "offline":
      return <OfflineView />;
    case "error":
      return <LoadErrorView />;
    case "ready":
      return <>{children}</>;
  }
}

function CenteredCard({ children }: { children: ReactNode }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center">
      {children}
    </div>
  );
}
