package importer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/bodylimit"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// importCap is the shipped default of DOCS_MAX_IMPORT_BODY_BYTES (B18.42: 25MB, decided);
// config/shipped_defaults_test.go pins that the default is this number.
const importCap = 25 << 20

// cappedImportChain mounts the import routes the way main.go does: inside a group that re-caps the
// body at the import limit.
func cappedImportChain(d *testutil.DB) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(bodylimit.Middleware(importCap, nil))
		r.Mount("/", impChain(d))
	})
	return r
}

// exportOf is a Notion export of one page plus an attachment of n random (incompressible) bytes,
// stored rather than deflated, so the upload is about n bytes long.
func exportOf(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("imported.md")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = f.Write([]byte("# Imported Doc\n\nbody text"))
	blob, err := zw.CreateHeader(&zip.FileHeader{Name: "attachment.bin", Method: zip.Store})
	if err != nil {
		t.Fatalf("zip create blob: %v", err)
	}
	noise := make([]byte, n)
	_, _ = rand.Read(noise)
	_, _ = blob.Write(noise)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// B18.42 — a 24MB import is imported; a 26MB one is refused with a sentence that says the limit,
// and creates nothing — whether its size is declared up front or only met while reading.
func TestImport_25MBLimit_24MBImports_26MBIsRefusedPlainly_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Imports", Slug: "imports-" + owner[len(owner)-6:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pages := func() int {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, sp.ID).Scan(&n); err != nil {
			t.Fatalf("count pages: %v", err)
		}
		return n
	}
	chain := cappedImportChain(d)

	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, importReq(t, "owner@corp.com", W, sp.ID, exportOf(t, 24<<20)))
	if rr.Code != http.StatusAccepted || pages() != 1 {
		t.Fatalf("a 24MB import = HTTP %d with %d pages, want 202 and 1 — %.200s", rr.Code, pages(), rr.Body.String())
	}

	for _, declared := range []bool{true, false} {
		req := importReq(t, "owner@corp.com", W, sp.ID, exportOf(t, 26<<20))
		if !declared {
			req.ContentLength = -1 // the overflow is met while reading, not refused up front
		}
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		var got struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(got.Error, "larger than 25 MB") {
			t.Errorf("a 26MB import (length declared: %v) = HTTP %d %q, want 413 saying it is larger than 25 MB",
				declared, rr.Code, got.Error)
		}
		if n := pages(); n != 1 {
			t.Errorf("a refused import (length declared: %v) left %d pages, want the 1 from before", declared, n)
		}
	}
}
