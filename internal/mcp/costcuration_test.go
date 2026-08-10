package mcp

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
)

// THE MCP PAGE PAYLOAD IS A CURATED SUBSET AND NOTHING NOTICED WHEN THE THING IT CURATES GREW.
//
// pageOut hand-picks 9 of model.Page's fields. That is deliberate — an LLM does not want the
// ProseMirror blob or the lock columns. But it means the MCP surface does not track the page: when
// migration 0018 SPLIT the single cost column into `ai_cost_usd` + `own_ai_cost_usd` and added the
// derived `total_ai_cost_usd`, pageOut kept the one field it already had, and the two new ones
// were unreachable through MCP by any tool for their whole life. Nothing was red. Nothing could
// be: the hand-picked list has no relationship to what it is picking FROM.
//
// This holds the COST fields of the two structs against each other so the next cost column cannot
// land on the page and quietly miss the surface AI agents read.
//
// ⚠ IT IS A SET COMPARED BOTH DIRECTIONS AGAINST A PINNED TABLE, not a subset check, because the
// two failure modes need different instruments and neither sees the other:
//
//   - a cost field ADDED to model.Page and not curated -> the model set gains a member the pinned
//     table does not have. A sweep of the source alone would just report a bigger number.
//   - a cost field DELETED from pageOut -> the mcp set loses a member the pinned table has. A
//     source-derived guard cannot see a deletion; only the pinned side can.
//
// ⚠ THE PINNED SETS ARE HARDCODED LITERALS, NOT DERIVED. A table that restated the reflected
// value would compare a constant to itself and pass for every value it could ever hold.
//
// ⚠ WHAT THIS DOES NOT CHECK, SAID RATHER THAN IMPLIED: it compares NAMES. A pageOut field
// correctly named `own_ai_cost_usd` and filled from the wrong column is invisible here — that is
// TestMCPGetPageReportsBothCostHalves_RealPG's job, which reads values off the wire. Neither guard
// subsumes the other and each has a mutation only it can see.
//
// If a future cost field is DELIBERATELY withheld from MCP, the fix is to split the pinned tables
// and write the reason on the line — a visible diff, not a silent gap.
func TestMCPPageOutCuratesEveryPageCostField(t *testing.T) {
	// The cost fields a page HAS, as the page JSON exposes them.
	pinnedModel := []string{"ai_cost_usd", "own_ai_cost_usd", "total_ai_cost_usd"}
	// The cost fields the MCP get_page tool exposes. Equal to the above: every cost a client can
	// read from the page JSON is reachable from the surface an AI agent reads.
	pinnedMCP := []string{"ai_cost_usd", "own_ai_cost_usd", "total_ai_cost_usd"}

	gotModel := costJSONNames(reflect.TypeOf(model.Page{}))
	gotMCP := costJSONNames(reflect.TypeOf(pageOut{}))

	if !reflect.DeepEqual(gotModel, pinnedModel) {
		t.Errorf("model.Page cost fields = %v, pinned %v.\n"+
			"A cost column was added to or removed from the page. If it was ADDED, decide whether "+
			"the MCP get_page tool must carry it (it almost certainly must — see this file's "+
			"header) and update BOTH tables; if it was REMOVED, update both tables.",
			gotModel, pinnedModel)
	}
	if !reflect.DeepEqual(gotMCP, pinnedMCP) {
		t.Errorf("mcp.pageOut cost fields = %v, pinned %v.\n"+
			"A cost field left the MCP page payload. An agent reading a document through MCP can "+
			"no longer see that half of what it cost.", gotMCP, pinnedMCP)
	}
	// The invariant the two tables exist to express. Stated separately so a future intentional
	// divergence has to edit this line and say why, rather than just widening a list.
	if !reflect.DeepEqual(pinnedModel, pinnedMCP) {
		t.Errorf("the pinned tables disagree: model %v vs mcp %v. Every cost a page has must be "+
			"reachable through MCP, or this line must be replaced by the reason it is not.",
			pinnedModel, pinnedMCP)
	}
}

// costJSONNames returns the sorted json names of every field of typ whose json name mentions cost.
// It reads the STRUCT TAG rather than the Go field name, because the tag is what reaches a client.
func costJSONNames(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if strings.Contains(name, "cost") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
