import { describe, expect, it, vi } from "vitest";
import { registerServiceWorker, shouldReloadOnControllerChange } from "./register";

// The app-side update flow. When a NEW service worker takes over an already-controlled page
// (a real deploy), we reload once so the open tab lands on the new bundle promptly — the
// "prompt handover" the fix wants. The footguns, encoded as tests:
//   - NEVER reload on first-ever install (there was no prior controller) — that would reload
//     every new visitor.
//   - NEVER reload more than once (the classic skipWaiting → controllerchange → reload loop).

describe("shouldReloadOnControllerChange", () => {
  it("reloads only when a controller already existed (a real update) and we haven't yet", () => {
    expect(shouldReloadOnControllerChange({ hadControllerAtStartup: true, hasReloaded: false })).toBe(true);
  });
  it("does NOT reload on first install (no prior controller)", () => {
    expect(shouldReloadOnControllerChange({ hadControllerAtStartup: false, hasReloaded: false })).toBe(false);
  });
  it("does NOT reload twice (loop guard)", () => {
    expect(shouldReloadOnControllerChange({ hadControllerAtStartup: true, hasReloaded: true })).toBe(false);
  });
});

// A minimal fake ServiceWorkerContainer.
function fakeSW(controller: object | null) {
  const listeners: Record<string, Array<() => void>> = {};
  return {
    controller,
    register: vi.fn().mockResolvedValue({}),
    addEventListener: (type: string, fn: () => void) => {
      (listeners[type] ??= []).push(fn);
    },
    // test helper: fire an event
    _emit: (type: string) => (listeners[type] ?? []).forEach((fn) => fn()),
  };
}

describe("registerServiceWorker", () => {
  it("does nothing outside production (no register, matching the pre-fix guard)", () => {
    const sw = fakeSW(null);
    registerServiceWorker({ isProd: false, serviceWorker: sw as any, reload: vi.fn() });
    expect(sw.register).not.toHaveBeenCalled();
  });

  it("registers /sw.js in production", () => {
    const sw = fakeSW(null);
    registerServiceWorker({ isProd: true, serviceWorker: sw as any, reload: vi.fn() });
    expect(sw.register).toHaveBeenCalledWith("/sw.js");
  });

  it("no-ops when the browser has no serviceWorker support", () => {
    // Must not throw.
    expect(() =>
      registerServiceWorker({ isProd: true, serviceWorker: undefined, reload: vi.fn() }),
    ).not.toThrow();
  });

  it("reloads once when a NEW worker takes over an already-controlled page (a deploy)", () => {
    const sw = fakeSW({}); // a controller already exists → returning user
    const reload = vi.fn();
    registerServiceWorker({ isProd: true, serviceWorker: sw as any, reload });

    sw._emit("controllerchange"); // the new SW claims the page
    expect(reload).toHaveBeenCalledTimes(1);

    sw._emit("controllerchange"); // a second event must NOT reload again — no loop
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("does NOT reload a first-time visitor (no controller at startup)", () => {
    const sw = fakeSW(null); // first-ever visit, no controller yet
    const reload = vi.fn();
    registerServiceWorker({ isProd: true, serviceWorker: sw as any, reload });

    sw._emit("controllerchange"); // the first SW claims the page — NOT an update
    expect(reload).not.toHaveBeenCalled();
  });
});
