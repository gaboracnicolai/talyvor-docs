package importer

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

// upload_bound_claim_test.go — WHAT maxUploadBytes ACTUALLY DOES, PINNED, BECAUSE ITS COMMENT
// SAID SOMETHING ELSE.
//
// ⚠ MEASURED (W3.54, tab-k4m7), not inferred: with `maxUploadBytes` temporarily set to 64 KiB,
// a 2 MiB import — THIRTY-TWO TIMES over it — was accepted with 202 and fully imported, through
// the real handler chain against real Postgres. The constant is `ParseMultipartForm`'s
// maxMemory argument. It is a MEMORY BUDGET for the multipart parser, not a size limit on the
// request: file parts past it are written to a temporary file on disk and the request proceeds.
//
// ⚠ THE COMMENT THAT WAS THERE CLAIMED THE OPPOSITE — "caps any single import to a manageable
// size so a malicious zip can't exhaust the box's memory". It does neither of those things, and
// the second clause is the one that matters: readUpload then reads the WHOLE part into `buf`
// with an unbounded loop, so the memory this constant was believed to protect is bounded by
// what the client chooses to send.
//
// ⚠⚠ WHETHER THERE SHOULD BE A REAL CAP IS A DECISION AND IS NOT TAKEN HERE. Adding one changes
// a shipping endpoint: imports that succeed today would begin to fail at some size. 200 MiB is
// already written down as the intended number, but "make the code do what the comment said" is
// still a behaviour change on a route people use, and it belongs to whoever owns that call.
// Filed separately. What is fixed here is the CLAIM, which was false, and this test is what
// stops it drifting back.

// TestParseMultipartForm_MaxMemoryDoesNotRejectOversizeBodies pins the standard-library
// semantics the old comment misread. It is deliberately a test of the MECHANISM rather than of
// our handler: driving the handler itself would need a body larger than the real 200 MiB
// constant, which readUpload would then hold in memory — a test nobody would keep.
func TestParseMultipartForm_MaxMemoryDoesNotRejectOversizeBodies(t *testing.T) {
	const tinyBudget = 1 << 10 // 1 KiB
	payload := bytes.Repeat([]byte("x"), 64<<10)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "export.zip")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/import/notion", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	if err := req.ParseMultipartForm(tinyBudget); err != nil {
		t.Fatalf("ParseMultipartForm(%d) on a %d-byte part returned %v.\n"+
			"If this now REJECTS, the standard library's contract changed and maxUploadBytes may "+
			"finally mean what its old comment claimed — re-read handler.go before celebrating.",
			tinyBudget, len(payload), err)
	}

	// And the part is fully readable afterwards: nothing was truncated to the budget either.
	f, _, err := req.FormFile("file")
	if err != nil {
		t.Fatalf("FormFile: %v", err)
	}
	defer f.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(f); err != nil {
		t.Fatalf("read part: %v", err)
	}
	if got.Len() != len(payload) {
		t.Fatalf("part read back as %d bytes, sent %d. maxMemory does not truncate either — it "+
			"decides only whether the bytes live in RAM or in a temp file", got.Len(), len(payload))
	}

	// NON-VACUITY: a payload that did NOT exceed the budget would make both assertions above
	// true for an uninteresting reason. This is what makes the test about the OVERSIZE case.
	if len(payload) <= tinyBudget {
		t.Fatalf("the payload (%d) does not exceed the budget (%d); this test would prove nothing",
			len(payload), tinyBudget)
	}
}
