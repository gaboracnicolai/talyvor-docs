package export

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// cap_reach_test.go — THE 50 MiB EXPORT CAP: THAT IT REFUSES, AND THAT THE HANDLER USES IT.
//
// ⚠ MEASURED (W3.53/W3.54, tab-k4m7, ~/talyvor-queue/w353-docs-reach-k4m7.py arm F13): raise
// the handler's `buf.cap` out of reach and the WHOLE SUITE STAYS GREEN. The cap is real —
// limitedBuffer.Write returns errExceedsLimit and the handler answers 413 — and nothing
// depended on it.
//
// ⚠⚠ THIS FILE IS TWO ASSERTIONS OF DIFFERENT STRENGTH AND SAYS SO, because pretending
// otherwise is how a guard gets over-read:
//
//	BEHAVIOURAL — limitedBuffer refuses the byte that would cross its cap, and accepts the one
//	              that reaches it exactly. That is the mechanism, exercised for real.
//	STRUCTURAL  — the export handler assigns MaxExportBytes to that buffer's cap, read out of
//	              handler.go's parse tree. This is WEAKER than driving the route, and it is
//	              here because driving it would need a page whose export exceeds 50 MiB: the
//	              Handler holds a concrete *Exporter rather than an interface, so no fake can
//	              be injected without changing production code to enable a test. That change
//	              may well be worth making; it is not a change to smuggle in behind a guard.
//
// Together they cover the two ways the cap stops working — the buffer stops refusing, or the
// handler stops handing it the declared number — which is what arm F13 mutates.

func TestLimitedBuffer_RefusesTheByteThatCrossesItsCap(t *testing.T) {
	const cap = 1024

	// Exactly at the cap is ACCEPTED. Without this half, a buffer that refused everything
	// would satisfy the refusal assertion below and be a worse product than no cap.
	var ok limitedBuffer
	ok.cap = cap
	if n, err := ok.Write(bytes.Repeat([]byte("x"), cap)); err != nil || n != cap {
		t.Fatalf("writing exactly the cap (%d) returned (%d, %v); the cap must ADMIT its own "+
			"value, not bite one byte early", cap, n, err)
	}

	// One byte more is REFUSED, and with the sentinel the handler maps to 413.
	var over limitedBuffer
	over.cap = cap
	if _, err := over.Write(bytes.Repeat([]byte("x"), cap+1)); err != errExceedsLimit {
		t.Fatalf("writing cap+1 returned %v, want errExceedsLimit — the handler keys its 413 on "+
			"that exact sentinel, so any other error becomes a 500 blaming the server for a "+
			"document the caller made too big", err)
	}

	// And the refusal is CUMULATIVE, not per-call: many small writes that add up past the cap
	// must also be refused. A per-call-only check would pass every assertion above while
	// letting an unbounded export through in chunks.
	var chunked limitedBuffer
	chunked.cap = cap
	var wroteErr error
	for i := 0; i < 100 && wroteErr == nil; i++ {
		_, wroteErr = chunked.Write(bytes.Repeat([]byte("y"), 64))
	}
	if wroteErr != errExceedsLimit {
		t.Fatalf("6400 bytes written in 64-byte chunks past a %d cap ended with %v, want "+
			"errExceedsLimit. An export is written in chunks, so a per-call bound would be no "+
			"bound at all", cap, wroteErr)
	}
	if chunked.Len() > cap {
		t.Fatalf("the buffer holds %d bytes with a cap of %d", chunked.Len(), cap)
	}
}

// TestExportHandler_WiresTheDeclaredCap is the structural half. It reads handler.go's parse
// tree for the assignment to the limited buffer's cap and requires it to be MaxExportBytes —
// so raising it to a literal, or pointing it at some other constant, is loud.
//
// ⚠ IT PARSES RATHER THAN GREPS: handler.go's own comment says "Anything past MaxExportBytes
// returns 413", so the identifier appears in prose as well as in code and a regex could match
// the sentence instead of the statement.
func TestExportHandler_WiresTheDeclaredCap(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "handler.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handler.go: %v", err)
	}

	var assignments []string
	ast.Inspect(f, func(n ast.Node) bool {
		as, isAssign := n.(*ast.AssignStmt)
		if !isAssign || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, isSel := as.Lhs[0].(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "cap" {
			return true
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent || ident.Name != "buf" {
			return true
		}
		switch rhs := as.Rhs[0].(type) {
		case *ast.Ident:
			assignments = append(assignments, rhs.Name)
		default:
			assignments = append(assignments, "<not a plain identifier>")
		}
		return true
	})

	// FLOOR: an assignment this test cannot find is an assignment it is not checking, and a
	// loop over zero matches passes silently. Measured at the commit that introduced this
	// file: exactly one.
	if len(assignments) == 0 {
		t.Fatal("no `buf.cap = …` assignment found in handler.go. Either the export buffer is " +
			"wired some other way now — in which case this test is checking nothing and must be " +
			"rewritten — or the parse is broken. Both are failures.")
	}
	for _, got := range assignments {
		if got != "MaxExportBytes" {
			t.Errorf("handler.go sets the export buffer's cap to %s, not MaxExportBytes. The "+
				"declared 50 MiB limit and the number actually enforced would then be two "+
				"different things, and the 413 message still says \"50MB\".",
				strings.TrimSpace(got))
		}
	}
}
