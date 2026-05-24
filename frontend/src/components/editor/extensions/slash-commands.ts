import { Plugin, PluginKey, EditorState, Transaction } from "prosemirror-state";
import { EditorView } from "prosemirror-view";
import { setBlockType, wrapIn } from "prosemirror-commands";
import { wrapInList } from "prosemirror-schema-list";
import { schema } from "../schema";

// SlashCommand describes one row in the menu. The action receives
// the editor view + the slash query range so it can delete the
// trigger before applying the transform.
export interface SlashCommand {
  id: string;
  group: string;
  label: string;
  hint?: string;
  // search target — defaults to the label, but commands like the
  // AI rows expose extra terms ("rewrite, edit, improve") so the
  // user can find them via synonyms.
  keywords?: string[];
  apply: (view: EditorView, from: number, to: number) => void;
}

// State the slash plugin carries between transactions. open=null when
// no menu is visible; otherwise it's the (start, end) range of the
// "/" trigger + filter text so the menu UI can position itself.
export interface SlashState {
  open: { from: number; to: number; query: string } | null;
}

export const slashKey = new PluginKey<SlashState>("slash");

// Helpers shared by command implementations. Each command first
// deletes the slash trigger range so the editor doesn't keep "/foo"
// in the document after the user picks an item.
const replaceWithBlock =
  (apply: (state: EditorState, dispatch: (tr: Transaction) => void) => boolean) =>
  (view: EditorView, from: number, to: number) => {
    let tr = view.state.tr.delete(from, to);
    view.dispatch(tr);
    apply(view.state, view.dispatch);
    view.focus();
  };

function setHeading(level: 1 | 2 | 3) {
  return setBlockType(schema.nodes.heading, { level });
}

function insertNode(view: EditorView, from: number, to: number, nodeName: string, attrs: Record<string, unknown> = {}) {
  const nt = schema.nodes[nodeName];
  if (!nt) return;
  const node = nt.createAndFill(attrs);
  if (!node) return;
  const tr = view.state.tr.delete(from, to).replaceSelectionWith(node);
  view.dispatch(tr);
  view.focus();
}

export const slashCommands: SlashCommand[] = [
  // Text -----------------------------------------------------------
  {
    id: "h1",
    group: "Text",
    label: "Heading 1",
    hint: "H1",
    apply: replaceWithBlock(setHeading(1)),
  },
  {
    id: "h2",
    group: "Text",
    label: "Heading 2",
    hint: "H2",
    apply: replaceWithBlock(setHeading(2)),
  },
  {
    id: "h3",
    group: "Text",
    label: "Heading 3",
    hint: "H3",
    apply: replaceWithBlock(setHeading(3)),
  },
  {
    id: "p",
    group: "Text",
    label: "Paragraph",
    hint: "Text",
    apply: replaceWithBlock(setBlockType(schema.nodes.paragraph)),
  },

  // Lists ----------------------------------------------------------
  {
    id: "ul",
    group: "Lists",
    label: "Bullet list",
    apply: replaceWithBlock(wrapInList(schema.nodes.bullet_list)),
  },
  {
    id: "ol",
    group: "Lists",
    label: "Numbered list",
    apply: replaceWithBlock(wrapInList(schema.nodes.ordered_list)),
  },
  {
    id: "todo",
    group: "Lists",
    label: "Todo list",
    keywords: ["checkbox", "task"],
    apply: (view, from, to) => {
      // Wrap the current block in a bullet list and stamp each
      // resulting list_item with checked=false so the CSS renders
      // the todo-style checkbox.
      let tr = view.state.tr.delete(from, to);
      view.dispatch(tr);
      wrapInList(schema.nodes.bullet_list)(view.state, view.dispatch);
      // Walk the selection's containing list_item and set checked.
      const sel = view.state.selection;
      const { $from } = sel;
      for (let d = $from.depth; d > 0; d--) {
        const node = $from.node(d);
        if (node.type === schema.nodes.list_item) {
          const pos = $from.before(d);
          view.dispatch(view.state.tr.setNodeMarkup(pos, undefined, { ...node.attrs, checked: false }));
          break;
        }
      }
      view.focus();
    },
  },

  // Content --------------------------------------------------------
  {
    id: "code",
    group: "Content",
    label: "Code block",
    apply: (view, from, to) => insertNode(view, from, to, "code_block"),
  },
  {
    id: "quote",
    group: "Content",
    label: "Blockquote",
    apply: replaceWithBlock(wrapIn(schema.nodes.blockquote)),
  },
  {
    id: "callout-info",
    group: "Content",
    label: "Callout (info)",
    apply: (view, from, to) => insertNode(view, from, to, "callout", { tone: "info" }),
  },
  {
    id: "callout-warning",
    group: "Content",
    label: "Callout (warning)",
    apply: (view, from, to) => insertNode(view, from, to, "callout", { tone: "warning" }),
  },
  {
    id: "callout-error",
    group: "Content",
    label: "Callout (error)",
    apply: (view, from, to) => insertNode(view, from, to, "callout", { tone: "error" }),
  },
  {
    id: "hr",
    group: "Content",
    label: "Divider",
    apply: (view, from, to) => insertNode(view, from, to, "horizontal_rule"),
  },

  // AI -- delegates to a future Lens-backed handler; the slash menu
  // surfaces the affordance now so users discover it. Each command
  // dispatches a "docs:ai-command" CustomEvent the App can listen
  // to and route into the actual call.
  ...aiCommand("ai-write", "✨ Write with AI...", "Generate from a prompt"),
  ...aiCommand("ai-summarize", "✨ Summarize above", "Condense the section above"),
  ...aiCommand("ai-grammar", "✨ Fix grammar", "Light proofreading pass"),
  ...aiCommand("ai-shorter", "✨ Make shorter", "Trim verbose sections"),
  ...aiCommand("ai-longer", "✨ Make longer", "Expand the current passage"),
  ...aiCommand("ai-translate", "✨ Translate...", "Translate selected text"),

  // Track integration ---------------------------------------------
  {
    id: "embed-issue",
    group: "Track",
    label: "🔗 Embed issue",
    keywords: ["issue", "track", "link"],
    apply: (view, from, to) => {
      const id = window.prompt("Issue identifier or ID:");
      if (!id) return;
      const node = schema.nodes.issue_embed.create({
        issue_id: id,
        identifier: id,
        title: "",
      });
      const tr = view.state.tr.delete(from, to).replaceSelectionWith(node);
      view.dispatch(tr);
      view.focus();
    },
  },
  {
    id: "insert-template",
    group: "Track",
    label: "📋 Insert template",
    apply: (view, from, to) => {
      // Phase 2: emit a custom event so the App can open the template
      // picker. Phase 3 will inline a Track template fetch.
      window.dispatchEvent(new CustomEvent("docs:open-templates"));
      view.dispatch(view.state.tr.delete(from, to));
    },
  },
];

function aiCommand(id: string, label: string, hint: string): SlashCommand[] {
  return [
    {
      id,
      group: "AI",
      label,
      hint,
      apply: (view, from, to) => {
        view.dispatch(view.state.tr.delete(from, to));
        window.dispatchEvent(
          new CustomEvent("docs:ai-command", { detail: { id } }),
        );
        view.focus();
      },
    },
  ];
}

// Filter commands by query — case-insensitive, matches label or
// any keyword. Returns commands in their original (group-sorted)
// order so the menu reads predictably.
export function filterSlashCommands(query: string): SlashCommand[] {
  const q = query.trim().toLowerCase();
  if (!q) return slashCommands;
  return slashCommands.filter((c) => {
    if (c.label.toLowerCase().includes(q)) return true;
    if (c.keywords?.some((k) => k.toLowerCase().includes(q))) return true;
    return false;
  });
}

// slashPlugin watches the document for a "/" trigger at the start of
// a textblock. While the trigger is active it tracks the query (the
// characters typed after the slash) and exposes the range so the
// React menu UI can position relative to it. We deliberately do NOT
// render any DOM here — that's the React layer's job; the plugin
// just exposes state.
export function slashPlugin(): Plugin<SlashState> {
  return new Plugin<SlashState>({
    key: slashKey,
    state: {
      init(): SlashState {
        return { open: null };
      },
      apply(tr) {
        // Recompute on every transaction. The regex match is O(line
        // length) and fires only when the user is mid-keystroke, so
        // the constant factor is invisible.
        const sel = tr.selection;
        const $from = sel.$from;
        if (!sel.empty) return { open: null };

        // Find the most recent "/" on the current text-block back to
        // the block start, with no intervening whitespace.
        const parent = $from.parent;
        if (!parent.isTextblock) return { open: null };
        const start = $from.start();
        const text = parent.textContent;
        const offset = $from.pos - start;
        const slice = text.slice(0, offset);
        const match = /\/([^\s/]*)$/.exec(slice);
        if (!match) return { open: null };
        const from = start + match.index;
        return {
          open: { from, to: $from.pos, query: match[1] },
        };
      },
    },
  });
}
