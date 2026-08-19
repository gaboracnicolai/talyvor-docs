package editsession_test

// THE "TAKE OVER" BUTTON IS RENDERED IN EXACTLY THE STATE IN WHICH THE TAKEOVER ROUTE IS
// GUARANTEED TO REFUSE, AND IS UNMOUNTED IN THE STATE IN WHICH IT WOULD WORK.
//
// `POST /v1/spaces/{s}/pages/{p}/edit-session/takeover` is `return s.claim(...)` — the SAME
// function `Acquire` calls, with the same arguments. `claim`'s UPSERT lands only when the slot is
// the caller's or the existing session has expired, so a LIVE foreign session is refused
// identically by both doors. The store's own comment says so plainly ("Semantically identical to
// Acquire on the safety-critical axis"); nothing downstream of it does.
//
// MEASURED THROUGH THE SHIPPED CHAIN (gatewayauth → authz → pageEnf) ON REAL POSTGRES, alice and
// bob both members of one workspace, bob holding an explicit space-level Edit grant:
//
//	state              POST …/edit-session      POST …/edit-session/takeover
//	live foreign       423 + {"error":…,"session":…}   423 + THE SAME BODY
//	expired            200 + the slot                  200 + the slot
//
// ⚠ AND THE SPA OFFERS THE BUTTON ON EXACTLY THE TOP ROW. `EditingBanner` returns null unless
// `flags.heldByOther`, and `sessionFlags` computes `heldByOther = live && holder && holder !== me`
// — the same predicate, on the same field, that makes the route answer 423. When the holder goes
// away the server flips `live` false, the observer's next poll (`refetchInterval: 10_000`) drops
// `heldByOther`, and the banner — with its button — is REMOVED FROM THE DOM. The page then becomes
// editable through ordinary auto-acquire, which needed no button at all. The only clicks that can
// succeed are the ones inside the ≤10s window between server-side expiry and the client noticing.
//
// ⚠ WHY THIS IS PINNED AND NOT FIXED: whether "Take over" should STEAL a live session — kick a
// colleague out of the editor mid-sentence — is a product call, not a session's. Both readings are
// defensible, and one of them is shipping without anyone having chosen it. THESE TESTS EXIST SO
// THE CHOICE CANNOT LAND SILENTLY: the moment takeover stops matching acquire, [PARITY-LIVE] reds
// and the decision is in the diff. If the product decides takeover SHOULD steal, this file is the
// one to change, deliberately.
//
// ⚠ AND THESE TWO GO CASES ARE NOT THE LOAD-BEARING HALF — MEASURED, NOT ASSUMED. Control T6
// blinds them and applies a stealing takeover on top, and the suite still REDS:
// TestEditSession_TakeoverOnlyWhenExpired_RealPG and five siblings already pin "a live session is
// not stolen". What is new here is only the byte-identical PARITY of the two doors, which nothing
// else asserts. **The unguarded half of this finding was the CLIENT one** — under control T3 the
// entire frontend suite reds exactly ONE test, the new EditingBanner.affordance.test.tsx case, so
// nothing could see a Takeover button rendered in the state the route grants. Said plainly here
// because a header that claimed these cases were the guard would be the same kind of overclaim
// the finding itself is about.
//
// The comments that claimed otherwise were corrected in the same commit — handler.go's
// respondClaim, useEditSession.ts's HEARTBEAT_MS note ("the page becomes claimable via Takeover"),
// and EditingBanner.tsx's own docstring, which described a state the component cannot be in.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

// twoWriterFixture: alice and bob, ONE workspace, bob granted space-level Edit — the ordinary
// "two colleagues on one document" setup, which is the only shape in which a takeover is
// meaningful at all. Without the grant every write op is a 403 from the enforcer and the
// measurement below would be about permissions, not about the writer slot.
type twoWriterFixture struct {
	d          *testutil.DB
	chain      http.Handler
	base       string
	page       string
	alice, bob string
	aliceEmail string
	bobEmail   string
}

func newTwoWriterFixture(t *testing.T) *twoWriterFixture {
	t.Helper()
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	pg := d.Page(t, ws, alice, "shared doc")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pg).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}
	if _, err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: spaceID,
		SubjectType: "member", SubjectID: bob,
		Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: alice,
	}); err != nil {
		t.Fatalf("grant edit to bob: %v", err)
	}
	return &twoWriterFixture{
		d: d, chain: v1EditSessionChain(d),
		base:  "/v1/spaces/" + spaceID + "/pages/" + pg + "/edit-session",
		page:  pg,
		alice: alice, bob: bob,
		aliceEmail: "alice@corp.com", bobEmail: "bob@corp.com",
	}
}

func (f *twoWriterFixture) do(method, path, email string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	f.chain.ServeHTTP(rr, esReq(method, path, email))
	return rr
}

// expire backdates the holder's heartbeat past DefaultTTL (30s) — the state a closed or crashed
// editor reaches on its own. Done in SQL rather than by sleeping so the test is not a clock.
func (f *twoWriterFixture) expire(t *testing.T) {
	t.Helper()
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE page_edit_sessions SET last_heartbeat = now() - interval '60 seconds' WHERE page_id = $1`,
		f.page); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}
}

// [PARITY-LIVE] the finding. With a LIVE foreign session — the state the SPA shows the button in
// — the takeover door and the acquire door answer identically, and neither takes the slot.
func TestEditSessionTakeover_OnALiveForeignSession_IsIndistinguishableFromAcquire_RealPG(t *testing.T) {
	f := newTwoWriterFixture(t)

	if rr := f.do(http.MethodPost, f.base, f.aliceEmail); rr.Code != http.StatusOK {
		t.Fatalf("alice acquire = %d %s, want 200 — the fixture's premise failed", rr.Code, rr.Body.String())
	}

	takeover := f.do(http.MethodPost, f.base+"/takeover", f.bobEmail)
	acquire := f.do(http.MethodPost, f.base, f.bobEmail)

	if takeover.Code != http.StatusLocked {
		t.Errorf("takeover on a LIVE foreign session = %d %s, want 423 — this is the state in which "+
			"EditingBanner renders the \"Take over\" button", takeover.Code, takeover.Body.String())
	}
	if takeover.Code != acquire.Code {
		t.Errorf("takeover = %d but acquire = %d — they are the same call (Store.Takeover is "+
			"`return s.claim(...)`, identical to Acquire); if they have diverged, the product "+
			"decision this file records has been made and this test is the place to say so",
			takeover.Code, acquire.Code)
	}
	if takeover.Body.String() != acquire.Body.String() {
		t.Errorf("takeover and acquire returned DIFFERENT bodies on a live foreign session:\n"+
			"  takeover: %s\n  acquire:  %s", takeover.Body.String(), acquire.Body.String())
	}

	// And the slot is still alice's — a live session is not stolen by either door.
	var holder string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT holder FROM page_edit_sessions WHERE page_id=$1`, f.page).Scan(&holder); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if holder != f.alice {
		t.Errorf("holder = %q after bob's takeover attempt, want alice (%q) — a LIVE session must "+
			"not be stolen", holder, f.alice)
	}
}

// [PARITY-EXPIRED] the other half, and the one that makes the finding a shape rather than a
// grumble: in the state where takeover DOES work, plain acquire works identically — so the button
// is not the only way in, and it is not on screen anyway (heldByOther is false once live is).
func TestEditSessionTakeover_OnAnExpiredSession_IsAlsoIndistinguishableFromAcquire_RealPG(t *testing.T) {
	f := newTwoWriterFixture(t)
	if rr := f.do(http.MethodPost, f.base, f.aliceEmail); rr.Code != http.StatusOK {
		t.Fatalf("alice acquire = %d, want 200", rr.Code)
	}

	// The observer sees live:false — which is precisely when the SPA stops rendering the button.
	f.expire(t)
	if rr := f.do(http.MethodGet, f.base, f.bobEmail); rr.Code != http.StatusOK {
		t.Fatalf("bob GET on an expired session = %d, want 200", rr.Code)
	} else if body := rr.Body.String(); !strings.Contains(body, `"live":false`) {
		t.Fatalf("bob GET body = %s, want live:false — the backdate did not expire the session, so "+
			"the claims below would be about the wrong state", body)
	}

	if rr := f.do(http.MethodPost, f.base+"/takeover", f.bobEmail); rr.Code != http.StatusOK {
		t.Errorf("takeover on an EXPIRED session = %d %s, want 200", rr.Code, rr.Body.String())
	}

	// Hand it back to alice, expire again, and come in through the ORDINARY door.
	if _, err := f.d.Pool.Exec(context.Background(),
		`UPDATE page_edit_sessions SET holder = $2, last_heartbeat = now() - interval '60 seconds'
         WHERE page_id = $1`, f.page, f.alice); err != nil {
		t.Fatalf("hand back: %v", err)
	}
	if rr := f.do(http.MethodPost, f.base, f.bobEmail); rr.Code != http.StatusOK {
		t.Errorf("ACQUIRE on an expired session = %d %s, want 200 — if this ever stops matching "+
			"takeover, the two doors have diverged and the affordance has become real",
			rr.Code, rr.Body.String())
	}
}
