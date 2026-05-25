import { Plugin } from "prosemirror-state";
import { Decoration, DecorationSet } from "prosemirror-view";
import { headingID } from "../TOC";

// headingAnchorPlugin stamps each heading node with a DOM id derived
// from its text content. The id matches the slug TOC entries emit,
// so `href="#auth-flow"` resolves to the live heading without us
// having to write IDs into the ProseMirror schema attrs.
//
// We use decorations (rather than mutating attrs) so the document
// stays canonical — anchors are presentation, not content.
export function headingAnchorPlugin(): Plugin {
  return new Plugin({
    props: {
      decorations(state) {
        const decos: Decoration[] = [];
        state.doc.descendants((node, pos) => {
          if (node.type.name !== "heading") return true;
          const id = headingID(node.textContent);
          if (!id) return true;
          // NodeDecoration applies the attribute to the rendered DOM
          // node itself. The +1 is unnecessary — pos points at the
          // node — but ProseMirror's `node` decoration takes (from, to)
          // where to = from + nodeSize.
          decos.push(
            Decoration.node(pos, pos + node.nodeSize, { id }),
          );
          return true;
        });
        return DecorationSet.create(state.doc, decos);
      },
    },
  });
}
