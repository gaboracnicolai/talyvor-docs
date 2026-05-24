import { history } from "prosemirror-history";
import { keymap } from "prosemirror-keymap";
import { baseKeymap, toggleMark, chainCommands, exitCode } from "prosemirror-commands";
import { undo, redo } from "prosemirror-history";
import { splitListItem, sinkListItem, liftListItem } from "prosemirror-schema-list";
import type { Plugin } from "prosemirror-state";

import { schema } from "../schema";
import { buildInputRules } from "./block-types";

// Build the keymap. Cmd / Ctrl shortcuts cover the basic formatting
// surface; the Enter / Tab / Shift+Tab keys also handle list
// behaviour via prosemirror-schema-list helpers.
function buildKeymap() {
  const mac = typeof navigator !== "undefined" && /Mac/.test(navigator.platform);
  const mod = mac ? "Meta" : "Ctrl";

  const bindings: Record<string, ReturnType<typeof toggleMark>> = {
    [`${mod}-b`]: toggleMark(schema.marks.strong),
    [`${mod}-i`]: toggleMark(schema.marks.em),
    [`${mod}-u`]: toggleMark(schema.marks.underline),
    [`${mod}-Shift-x`]: toggleMark(schema.marks.strike),
    [`${mod}-e`]: toggleMark(schema.marks.code),
    [`${mod}-Shift-h`]: toggleMark(schema.marks.highlight),
  };

  return keymap({
    ...bindings,
    [`${mod}-z`]: undo,
    [`${mod}-Shift-z`]: redo,
    [`${mod}-y`]: redo,
    Enter: splitListItem(schema.nodes.list_item),
    Tab: sinkListItem(schema.nodes.list_item),
    "Shift-Tab": liftListItem(schema.nodes.list_item),
    // Allow Shift+Enter to escape a code block (otherwise the user
    // can get stuck inside).
    "Shift-Enter": chainCommands(exitCode, (state, dispatch) => {
      if (!dispatch) return false;
      dispatch(
        state.tr
          .replaceSelectionWith(schema.nodes.hard_break.create())
          .scrollIntoView(),
      );
      return true;
    }),
  });
}

// buildPlugins returns the bundled plugin list passed to EditorState
// at construction. Custom plugins (slash menu, floating toolbar) are
// added by Editor.tsx since they own React-owned UI surfaces.
export function buildPlugins(extras: Plugin[] = []): Plugin[] {
  return [
    history(),
    buildInputRules(),
    buildKeymap(),
    keymap(baseKeymap),
    ...extras,
  ];
}

export { schema };
