import type { Config } from "tailwindcss";

// Talyvor brand v4 tokens, the same as Talyvor Track. The values live in
// src/styles/tokens.css (a verbatim copy of the brand folder's tokens.css);
// every class here reads a --tv-* variable. color-mix keeps Tailwind's
// opacity modifiers (bg-accent/30, hover:bg-border/40) working on a
// variable colour.
const tv = (name: string) =>
  `color-mix(in srgb, var(--tv-${name}) calc(<alpha-value> * 100%), transparent)`;

const config: Config = {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        bg: tv("canvas"),
        surface: tv("surface"),
        raised: tv("raised"),
        border: tv("line"),
        "border-strong": tv("line-strong"),
        text: tv("ink"),
        muted: tv("ink-muted"),
        label: tv("label"),
        accent: tv("accent"),
        "accent-hover": tv("accent-hover"),
        "on-accent": tv("on-accent"),
        "accent-tint": tv("accent-tint"),
        callout: {
          info: tv("accent"),
          warning: tv("caution"),
          error: tv("critical"),
          success: tv("positive"),
        },
      },
      fontFamily: {
        mono: ["IBM Plex Mono", "ui-monospace", "monospace"],
        sans: ["Space Grotesk", "system-ui", "sans-serif"],
      },
    },
  },
  plugins: [],
};

export default config;
