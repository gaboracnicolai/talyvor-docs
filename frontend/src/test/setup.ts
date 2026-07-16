import "@testing-library/jest-dom/vitest";

// jsdom's localStorage can be flaky across node/vitest versions (a Node experimental
// localStorage shadows window's). Pin a deterministic in-memory implementation so tests that
// mount real components — which read docs_api_key / docs_workspace_id — behave the same
// everywhere instead of throwing "getItem of undefined" from async query functions.
if (typeof globalThis.localStorage === "undefined" || !globalThis.localStorage?.getItem) {
  const store = new Map<string, string>();
  const mem: Storage = {
    getItem: (k) => (store.has(k) ? store.get(k)! : null),
    setItem: (k, v) => void store.set(k, String(v)),
    removeItem: (k) => void store.delete(k),
    clear: () => store.clear(),
    key: (i) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  };
  Object.defineProperty(globalThis, "localStorage", { value: mem, configurable: true });
}
