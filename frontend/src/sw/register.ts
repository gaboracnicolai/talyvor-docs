// Service-worker registration + a loop-safe update→reload path.
//
// The service worker itself (public/sw.js) now serves the app shell network-first, so a
// returning user's NEXT navigation or reload already gets the current version — that is the
// root fix for the stale-shell routing hazard. This module adds the "prompt handover" on top:
// when a NEW worker takes over an ALREADY-controlled page (a real deploy), reload the open
// tab once so it doesn't linger on the old bundle.
//
// Two footguns, both guarded here and pinned by register.test.ts:
//   - reloading on first-ever install (there was no prior controller) would reload every new
//     visitor — so we only reload when a controller already existed at startup;
//   - the classic skipWaiting → controllerchange → reload LOOP — so we reload at most once.

export interface ReloadDecision {
  hadControllerAtStartup: boolean;
  hasReloaded: boolean;
}

// Pure decision, unit-tested. Reload iff this is a real update (a controller already
// controlled the page) and we haven't reloaded yet.
export function shouldReloadOnControllerChange(state: ReloadDecision): boolean {
  return state.hadControllerAtStartup && !state.hasReloaded;
}

export interface RegisterDeps {
  isProd: boolean;
  serviceWorker: ServiceWorkerContainer | undefined;
  reload: () => void;
}

export function registerServiceWorker({ isProd, serviceWorker, reload }: RegisterDeps): void {
  // Production only — Vite's dev server doesn't serve /sw.js like a real deploy, and a dev SW
  // would intercept HMR. Same guard the pre-fix code had.
  if (!isProd || !serviceWorker) return;

  // Snapshot at startup: was the page already controlled? If yes, a later controllerchange
  // means a NEW worker took over (an update). If no, it's just the first worker claiming the
  // page — not an update, so we must not reload.
  const hadControllerAtStartup = !!serviceWorker.controller;
  let hasReloaded = false;

  serviceWorker.addEventListener("controllerchange", () => {
    if (shouldReloadOnControllerChange({ hadControllerAtStartup, hasReloaded })) {
      hasReloaded = true;
      reload();
    }
  });

  // Registration failures are non-fatal — the app works online-only.
  serviceWorker.register("/sw.js").catch(() => {});
}
