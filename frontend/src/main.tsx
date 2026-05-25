import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { App } from "./App";
import { syncManager } from "./lib/sync";
import "./index.css";

// Single app-wide query client. staleTime 30s matches the cadence at
// which a typical editor session re-reads a page; refetchOnWindowFocus
// is off because we already auto-save on change.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: 30_000, retry: 1, refetchOnWindowFocus: false },
  },
});

// Service worker registration. Only runs in production builds —
// Vite's dev server doesn't serve /sw.js the way a real deploy does,
// and a stale dev SW would intercept HMR requests.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Registration failures are non-fatal — the app works
      // online-only. We don't surface them to the user.
    });
  });
}

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
