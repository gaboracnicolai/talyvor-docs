package export

// A FAILED CHILD LISTING WAS DISCARDED, AND THE EXPORT CAME BACK AS A COMPLETE DOCUMENT.
//
// gatherPages read the children and threw the error away:
//
//	siblings, err := e.listChildren(ctx, root)
//	if err != nil {
//	    return out, nil        // <- `out` is the root, alone. err is dropped. nil error returned.
//	}
//
// So a timeout, a dropped connection, a statement cancellation — anything that makes ONE query
// fail while the root read (a separate, earlier query) succeeded — produced a 200 carrying a
// document with its children missing and nothing saying so. The user who asked for a page AND its
// children got a page. `handler.Export` maps a non-`ErrNotFound` error to 500, so the honest
// answer was already wired and reachable; nothing was ever handed to it.
//
// ⚠ THIS IS #134'S FINDING THROUGH A DIFFERENT DOOR, AND #134 DID NOT CLOSE IT. That merge fixed
// WHICH ROWS the query asks for; this is what happens when the query does not answer at all. Both
// end at the same false statement, and the package's own rule — WithPageRead's comment, that an
// unwired gate must be an ERROR rather than "an export with the children quietly dropped",
// because silently omitting children from a document a user asked to export WITH them "is its own
// false statement" — condemns this one in the same words. ErrNoPageReadGate is the shape the
// answer already takes when the OTHER precondition of the expansion is unmet.
//
// ⚠ AND THE ROOT-ONLY EXPORT IS NOT A DEGRADED ANSWER, IT IS A WRONG ONE. A markdown file with a
// root and no children is indistinguishable from the correct export of a childless page. There is
// no marker, no header and no status to tell them apart, so nothing downstream — a user, a backup
// job, a migration script reading these files — can detect the loss.
//
// THE TAGS:
//
//	[PREMISE-WORKS]   the same fake exports root+child when List succeeds  ← else the error case proves nothing
//	[ERROR-SURFACED]  a failing child listing returns an error, per format ← the defect
//	[ERROR-WRAPPED]   ... and it carries the store's own error             ← a 500 whose cause is retrievable
//	[ROOT-ONLY-OK]    include_children=false still exports without the read ← the fix must not widen the failure

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

// errListPages is fakePages with one addition: List can fail. Everything else — including the
// scoped root read the expansion depends on — behaves exactly as the passing fake does, so the
// only difference between [PREMISE-WORKS] and [ERROR-SURFACED] is whether one query answered.
type errListPages struct {
	*fakePages
	listErr error
}

func (e *errListPages) List(ctx context.Context, filter page.PageFilter) ([]model.Page, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.fakePages.List(ctx, filter)
}

var errChildListBlewUp = errors.New("page: list: server closed the connection unexpectedly")

func newChildErrExporter(listErr error) *Exporter {
	child := "pg-root"
	base := &fakePages{
		byID: map[string]*model.Page{
			"pg-root":  makePage("pg-root", "Board Pack", samplePM, nil, 1),
			"pg-child": makePage("pg-child", "Agenda", samplePM, &child, 2),
		},
		bySpace: map[string][]model.Page{
			"sp-1": {
				*makePage("pg-root", "Board Pack", samplePM, nil, 1),
				*makePage("pg-child", "Agenda", samplePM, &child, 2),
			},
		},
	}
	return newExporter(&errListPages{fakePages: base, listErr: listErr}, &fakeSpaces{}).
		WithPageRead(allowAllPages{})
}

// exportTo drives the SHIPPED per-format entry point so all four are covered by the same
// assertion — they each call gatherPages, and a fix applied to one of them is not a fix.
func exportTo(e *Exporter, format Format, includeChildren bool) (string, error) {
	var buf bytes.Buffer
	err := e.ExportPage(context.Background(), "pg-root", []string{"ws-1"},
		ExportOptions{Format: format, IncludeChildren: includeChildren}, &buf)
	return buf.String(), err
}

func TestExport_ChildListingFailure_IsNotASilentlyTruncatedDocument(t *testing.T) {
	formats := []Format{FormatMD, FormatHTML, FormatPDF, FormatDocx}

	// ── PREMISE. With the identical fake and no error, the child IS in the export. Without this,
	// an "error was returned" assertion below could pass on a fake that never had a child.
	ok := newChildErrExporter(nil)
	for _, f := range formats {
		body, err := exportTo(ok, f, true)
		if err != nil {
			t.Fatalf("[PREMISE-WORKS] %s export failed with a healthy store: %v", f, err)
		}
		if f == FormatMD && !strings.Contains(body, "Agenda") {
			t.Fatalf("[PREMISE-WORKS] the markdown export of a healthy store does not contain the "+
				"child's title, so the fake has no child to lose:\n%s", body)
		}
	}

	// ── the defect: one query fails, every format must say so.
	broken := newChildErrExporter(errChildListBlewUp)
	for _, f := range formats {
		body, err := exportTo(broken, f, true)
		if err == nil {
			t.Errorf("[ERROR-SURFACED] %s export returned NO ERROR when the child listing failed — "+
				"the caller asked for a page and its children, one of the two reads failed, and the "+
				"answer is a document containing the root alone (%d bytes). handler.Export maps a "+
				"non-ErrNotFound error to 500; nothing was handed to it, so this reached the user as "+
				"a 200 with a complete-looking file. A root-only export is indistinguishable from the "+
				"correct export of a childless page: no marker, no header, nothing downstream can "+
				"detect the loss.", f, len(body))
			continue
		}
		if !errors.Is(err, errChildListBlewUp) {
			t.Errorf("[ERROR-WRAPPED] %s export failed with %v, which does not wrap the store's own "+
				"error — a 500 whose cause cannot be retrieved from the error chain is a second "+
				"thing the operator has to guess at", f, err)
		}
	}

	// ── the fix must not widen the failure: a root-only export never consults the child listing at
	// all, so a broken listing is none of its business.
	for _, f := range formats {
		if _, err := exportTo(broken, f, false); err != nil {
			t.Errorf("[ROOT-ONLY-OK] %s export with include_children=false failed (%v) — that request "+
				"does not read the children, so a failing child listing must not reach it", f, err)
		}
	}
}
