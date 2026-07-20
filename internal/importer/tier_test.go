package importer_test

// A3 tier enforcement for IMPORT. POST /v1/import/{confluence,notion} creates pages in a space named in
// the multipart FORM, gated (pre-fix) only by AuthorizeWorkspace(workspace_id) — never the target space's
// AccessEdit that page.Create enforces at the canonical door. So a view-only member could bulk-import
// pages into a space they may only view. RED (handler ungated): the viewer's import creates pages — assert
// on pages in the DB. GREEN (spaceauth gate): viewer 403 + no pages; editor succeeds; a space_id in another
// workspace is 404 (no oracle).

import (
	"archive/zip"
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/importer"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

const impSecret = "sec4-test-gateway-secret-0123456789"

func impChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	svc := importer.New(page.NewStore(d.Pool), spaceStore)
	h := importer.NewHandler(svc).WithAccess(spaceauth.New(spaceStore, permStore))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(impSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func notionZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("imported.md")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := f.Write([]byte("# Imported Doc\n\nbody text")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func importReq(t *testing.T, email, wsID, spaceID string, zipBytes []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("workspace_id", wsID)
	_ = mw.WriteField("space_id", spaceID)
	fw, err := mw.CreateFormFile("file", "export.zip")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(zipBytes); err != nil {
		t.Fatalf("write zip: %v", err)
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/import/notion", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-Gateway-Auth", impSecret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func TestA3_Import_TierEnforcement(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	viewer := d.Member(t, W, "viewer@corp.com")
	editor := d.Member(t, W, "editor@corp.com")

	targetSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Target", Slug: "target-" + owner[len(owner)-6:], Private: true, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed target space: %v", err)
	}
	grant := func(subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: targetSpace.ID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(viewer, permission.AccessView)
	grant(editor, permission.AccessEdit)

	W2 := d.Workspace(t)
	other := d.Member(t, W2, "other@corp.com")
	foreignSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W2, Name: "Foreign", Slug: "foreign-" + other[len(other)-6:], CreatedBy: other,
	})
	if err != nil {
		t.Fatalf("seed foreign space: %v", err)
	}

	chain := impChain(d)
	zipBytes := notionZip(t)
	code := func(r *http.Request) int { rr := httptest.NewRecorder(); chain.ServeHTTP(rr, r); return rr.Code }
	pagesIn := func(spaceID string) int {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, spaceID).Scan(&n); err != nil {
			t.Fatalf("count pages: %v", err)
		}
		return n
	}

	// ── (a) RED: a view-tier member must be REFUSED and NO pages imported into the target space. ──
	if c := code(importReq(t, "viewer@corp.com", W, targetSpace.ID, zipBytes)); c != http.StatusForbidden {
		t.Errorf("viewer import = %d, want 403 (view-tier cannot import into the space)", c)
	}
	if n := pagesIn(targetSpace.ID); n != 0 {
		t.Errorf("viewer import CREATED %d page(s) despite the view tier", n)
	}

	// ── (b) POSITIVE: an edit-tier member succeeds and pages ARE imported. ──
	if c := code(importReq(t, "editor@corp.com", W, targetSpace.ID, zipBytes)); c != http.StatusAccepted {
		t.Errorf("editor import = %d, want 202 (edit grant)", c)
	}
	if n := pagesIn(targetSpace.ID); n == 0 {
		t.Errorf("editor import created no pages")
	}

	// ── (c) Cross-workspace: a space_id in another workspace → 404 (no oracle), and no pages created. ──
	if c := code(importReq(t, "viewer@corp.com", W, foreignSpace.ID, zipBytes)); c != http.StatusNotFound {
		t.Errorf("import into a foreign-workspace space = %d, want 404 (no oracle)", c)
	}
	if n := pagesIn(foreignSpace.ID); n != 0 {
		t.Errorf("import created %d page(s) in a foreign-workspace space", n)
	}
}
