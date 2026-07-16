import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { App } from "./App";
import { syncManager } from "./lib/sync";
import { registerServiceWorker } from "./sw/register";
import "./index.css";

// Single app-wide query client. staleTime 30s matches the cadence at
// which a typical editor session re-reads a page; refetchOnWindowFocus
// is off because we already auto-save on change.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 30_000, retry: 1, refetchOnWindowFocus: false },
  },
});

// Service worker registration + loop-safe update→reload (src/sw/register.ts). Production
// only, same as before — but now, when a new worker takes over an already-controlled page
// after a deploy, the tab reloads once onto the new bundle (guarded against the first-install
// and infinite-loop footguns). The SW itself serves the app shell network-first, so this is
// the prompt-handover layer on top of the root fix.
window.addEventListener("load", () => {
  registerServiceWorker({
    isProd: import.meta.env.PROD,
    serviceWorker: "serviceWorker" in navigator ? navigator.serviceWorker : undefined,
    reload: () => window.location.reload(),
  });
});

// SyncManager flushes the offline write queue. Starts immediately
// so a returning user with queued writes from a previous session
// re-syncs on first load.
syncManager.startAutoSync();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </React.StrictMode>,
);
