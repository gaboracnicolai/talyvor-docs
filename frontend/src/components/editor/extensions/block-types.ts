import {
  inputRules,
  textblockTypeInputRule,
  wrappingInputRule,
  InputRule,
} from "prosemirror-inputrules";
import type { NodeType } from "prosemirror-model";
import { schema } from "../schema";

// Input rules let the user type markdown shortcuts and have the
// editor convert them into ProseMirror nodes:
//   #    → h1     ##   → h2     ###  → h3
//   -    → bullet_list  / *  → bullet_list
//   1.   → ordered_list
//   >    → blockquote
//   ```  → code_block
//   ---  → horizontal_rule
//
// Each rule matches a prefix at the start of a block and replaces the
// matched text with the new node — the user's typing flow stays
// uninterrupted.

function headingRule(nodeType: NodeType, maxLevel: number): InputRule {
  return textblockTypeInputRule(
    new RegExp("^(#{1," + maxLevel + "})\\s$"),
    nodeType,
    (match) => ({ level: match[1].length }),
  );
}

function blockquoteRule(nodeType: NodeType): InputRule {
  return wrappingInputRule(/^\s*>\s$/, nodeType);
}

function bulletListRule(nodeType: NodeType): InputRule {
  return wrappingInputRule(/^\s*([-+*])\s$/, nodeType);
}

function orderedListRule(nodeType: NodeType): InputRule {
  return wrappingInputRule(
    /^(\d+)\.\s$/,
    nodeType,
    (match) => ({ order: +match[1] }),
    (match, node) => node.childCount + node.attrs.order === +match[1],
  );
}

function codeBlockRule(nodeType: NodeType): InputRule {
  return textblockTypeInputRule(/^```([a-z]*)\s$/, nodeType, (match) => ({
    language: match[1] ?? "",
  }));
}

// horizontalRuleRule: typing `---` followed by Enter replaces the
// line with an <hr>. Implemented as a custom InputRule because the
// node has no content (textblockTypeInputRule expects a textblock).
function horizontalRuleRule(nodeType: NodeType): InputRule {
  return new InputRule(/^---$/, (state, _match, start, end) => {
    const tr = state.tr.delete(start, end).replaceSelectionWith(nodeType.create());
    return tr;
  });
}

// buildInputRules returns the combined plugin. Order doesn't matter
// — input rules are checked in registration order, but each rule's
// regex is specific enough that overlap is rare.
export function buildInputRules() {
  const rules = [
    headingRule(schema.nodes.heading, 3),
    blockquoteRule(schema.nodes.blockquote),
    bulletListRule(schema.nodes.bullet_list),
    orderedListRule(schema.nodes.ordered_list),
    codeBlockRule(schema.nodes.code_block),
    horizontalRuleRule(schema.nodes.horizontal_rule),
  ];
  return inputRules({ rules });
}
