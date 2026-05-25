import { Schema } from "prosemirror-model";
import type { NodeSpec, MarkSpec } from "prosemirror-model";

// Talyvor Docs schema. Exported as a top-level `schema` so the
// real-time collaboration layer that lands in Phase 3 can re-use
// the exact same node + mark definitions — Yjs sync depends on
// both ends sharing the schema down to the attribute level.
//
// Block nodes inherit from the prosemirror-schema-basic shape but
// add Docs-specific extras: heading levels capped at 3, todo list
// items, code_block with a language attribute, callouts with a
// tone, and a thin issue_embed inline node for Track integration.

const nodes: { [name: string]: NodeSpec } = {
  doc: { content: "block+" },

  paragraph: {
    content: "inline*",
    group: "block",
    parseDOM: [{ tag: "p" }],
    toDOM: () => ["p", 0],
  },

  blockquote: {
    content: "block+",
    group: "block",
    defining: true,
    parseDOM: [{ tag: "blockquote" }],
    toDOM: () => ["blockquote", 0],
  },

  horizontal_rule: {
    group: "block",
    parseDOM: [{ tag: "hr" }],
    toDOM: () => ["hr"],
  },

  heading: {
    attrs: { level: { default: 1 } },
    content: "inline*",
    group: "block",
    defining: true,
    parseDOM: [
      { tag: "h1", attrs: { level: 1 } },
      { tag: "h2", attrs: { level: 2 } },
      { tag: "h3", attrs: { level: 3 } },
    ],
    toDOM: (node) => [`h${node.attrs.level}`, 0],
  },

  code_block: {
    attrs: { language: { default: "" } },
    content: "text*",
    marks: "",
    group: "block",
    code: true,
    defining: true,
    parseDOM: [
      {
        tag: "pre",
        preserveWhitespace: "full",
        getAttrs: (n) => ({
          language: (n as HTMLElement).getAttribute("data-language") ?? "",
        }),
      },
    ],
    toDOM: (node) => [
      "pre",
      { "data-language": node.attrs.language },
      ["code", 0],
    ],
  },

  text: { group: "inline" },

  image: {
    inline: true,
    group: "inline",
    draggable: true,
    attrs: {
      src: {},
      alt: { default: "" },
      title: { default: "" },
    },
    parseDOM: [
      {
        tag: "img[src]",
        getAttrs: (n) => {
          const el = n as HTMLImageElement;
          return {
            src: el.getAttribute("src") ?? "",
            alt: el.getAttribute("alt") ?? "",
            title: el.getAttribute("title") ?? "",
          };
        },
      },
    ],
    toDOM: (node) => [
      "img",
      { src: node.attrs.src, alt: node.attrs.alt, title: node.attrs.title },
    ],
  },

  hard_break: {
    inline: true,
    group: "inline",
    selectable: false,
    parseDOM: [{ tag: "br" }],
    toDOM: () => ["br"],
  },

  // Lists. We embed prosemirror-schema-list's behaviour but inline
  // the shapes here so the schema is fully self-describing for
  // collab serialisation.
  ordered_list: {
    content: "list_item+",
    group: "block",
    attrs: { order: { default: 1 } },
    parseDOM: [
      {
        tag: "ol",
        getAttrs: (n) => {
          const el = n as HTMLOListElement;
          const start = el.getAttribute("start");
          return { order: start ? +start : 1 };
        },
      },
    ],
    toDOM: (node) =>
      node.attrs.order === 1
        ? ["ol", 0]
        : ["ol", { start: node.attrs.order }, 0],
  },

  bullet_list: {
    content: "list_item+",
    group: "block",
    parseDOM: [{ tag: "ul" }],
    toDOM: () => ["ul", 0],
  },

  // list_item is shared between ordered_list and bullet_list, plus
  // it carries an optional `checked` attribute to power todo items
  // (rendered with a custom class so CSS controls the checkbox UX).
  list_item: {
    content: "paragraph block*",
    defining: true,
    attrs: { checked: { default: null } },
    parseDOM: [
      {
        tag: "li",
        getAttrs: (n) => {
          const el = n as HTMLElement;
          const c = el.getAttribute("data-checked");
          if (c === null) return {};
          return { checked: c === "true" };
        },
      },
    ],
    toDOM: (node) => {
      const checked = node.attrs.checked;
      if (checked === null) return ["li", 0];
      return [
        "li",
        { class: "todo", "data-checked": String(checked) },
        0,
      ];
    },
  },

  // Callout: coloured block with one of four tones. defining=true so
  // the user can paste a paragraph into it without losing the wrapper.
  callout: {
    content: "block+",
    group: "block",
    defining: true,
    attrs: { tone: { default: "info" } },
    parseDOM: [
      {
        tag: "div.callout",
        getAttrs: (n) => ({
          tone: (n as HTMLElement).getAttribute("data-tone") ?? "info",
        }),
      },
    ],
    toDOM: (node) => [
      "div",
      { class: "callout", "data-tone": node.attrs.tone },
      0,
    ],
  },

  // issue_embed renders inline as a chip. The `id` attribute is the
  // Track issue UUID; the label can be filled lazily by a node view.
  issue_embed: {
    inline: true,
    group: "inline",
    atom: true,
    attrs: {
      issue_id: {},
      identifier: { default: "" },
      title: { default: "" },
    },
    parseDOM: [
      {
        tag: "span.issue-embed",
        getAttrs: (n) => {
          const el = n as HTMLElement;
          return {
            issue_id: el.getAttribute("data-issue") ?? "",
            identifier: el.getAttribute("data-identifier") ?? "",
            title: el.getAttribute("data-title") ?? "",
          };
        },
      },
    ],
    toDOM: (node) => [
      "span",
      {
        class: "issue-embed",
        "data-issue": node.attrs.issue_id,
        "data-identifier": node.attrs.identifier,
        "data-title": node.attrs.title,
      },
      node.attrs.identifier || node.attrs.issue_id,
    ],
  },

  // database_block is a block-level placeholder that defers all
  // rendering to a React node view. The PM node carries only the
  // database_id; the node view fetches schema + rows + views on its
  // own and renders the table / kanban / list / gallery surface.
  database_block: {
    group: "block",
    atom: true,
    selectable: true,
    attrs: {
      database_id: { default: "" },
    },
    parseDOM: [
      {
        tag: "div.database-block",
        getAttrs: (n) => {
          const el = n as HTMLElement;
          return { database_id: el.getAttribute("data-database") ?? "" };
        },
      },
    ],
    toDOM: (node) => [
      "div",
      {
        class: "database-block",
        "data-database": node.attrs.database_id,
      },
    ],
  },
};

const marks: { [name: string]: MarkSpec } = {
  link: {
    attrs: { href: {}, title: { default: null } },
    inclusive: false,
    parseDOM: [
      {
        tag: "a[href]",
        getAttrs: (n) => {
          const el = n as HTMLAnchorElement;
          return {
            href: el.getAttribute("href") ?? "",
            title: el.getAttribute("title"),
          };
        },
      },
    ],
    toDOM: (mark) => [
      "a",
      { href: mark.attrs.href, title: mark.attrs.title, rel: "noopener noreferrer", target: "_blank" },
      0,
    ],
  },
  em: {
    parseDOM: [{ tag: "i" }, { tag: "em" }, { style: "font-style=italic" }],
    toDOM: () => ["em", 0],
  },
  strong: {
    parseDOM: [
      { tag: "strong" },
      { tag: "b", getAttrs: (n) => (n as HTMLElement).style.fontWeight !== "normal" && null },
      { style: "font-weight=bold" },
    ],
    toDOM: () => ["strong", 0],
  },
  underline: {
    parseDOM: [{ tag: "u" }, { style: "text-decoration=underline" }],
    toDOM: () => ["u", 0],
  },
  strike: {
    parseDOM: [{ tag: "s" }, { tag: "del" }, { style: "text-decoration=line-through" }],
    toDOM: () => ["s", 0],
  },
  code: {
    parseDOM: [{ tag: "code" }],
    toDOM: () => ["code", 0],
  },
  highlight: {
    parseDOM: [{ tag: "mark" }],
    toDOM: () => ["mark", 0],
  },
};

export const schema = new Schema({ nodes, marks });
