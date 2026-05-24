import type { Config } from "tailwindcss";

// Same dark-mode design tokens as Talyvor Track + Lens. The IBM Plex
// Mono / Inter pairing is part of the Talyvor brand language.
const config: Config = {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        bg: "#0c0e12",
        surface: "#13161c",
        border: "#1e2330",
        text: "#d4d8e2",
        muted: "#8892a4",
        accent: "#f0a030",
        callout: {
          info: "#3b82f6",
          warning: "#f59e0b",
          error: "#ef4444",
          success: "#22c55e",
        },
      },
      fontFamily: {
        mono: ["IBM Plex Mono", "ui-monospace", "monospace"],
        sans: ["Inter", "system-ui", "sans-serif"],
      },
    },
  },
  plugins: [],
};

export default config;
