import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Vitest config for the frontend. jsdom gives component/route tests a DOM; the pure
// path/guard tests need no DOM but run in the same suite. This is SEPARATE from vite.config
// so the app build is unaffected.
export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "~": path.resolve(__dirname, "src") } },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
