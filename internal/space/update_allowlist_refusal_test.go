package space

import (
	"context"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// update_allowlist_refusal_test.go — THE HALF OF THE ALLOW-LIST NOTHING TESTED.
//
// Store.Update takes the PATCH request body verbatim (Handler.Update decodes
// `map[string]any` straight from r.Body — there is no struct and therefore no tag set) and
// builds SQL by INTERPOLATING the caller's key:
//
//	set = append(set, fmt.Sprintf("%s = $%d", k, n))
//
// The value is bound; the KEY is not. `updatable` is the only thing between a request key
// and a SQL identifier, which is why the statement carries a //nosemgrep.
//
// ⚠ MEASURED (W3.52, tab-k4m7, ~/talyvor-queue/w352-allowlist-reach-k4m7.py): DISABLE the
// allow-list check and THE WHOLE SUITE STAYS GREEN. The same probe against
// internal/page's equivalent goes RED (TestSec_Update_AICostIsNotWritableFromTheRequestBody),
// so this was one package of two, not a repo-wide gap.
//
// ⚠⚠ AND THE REASON IS A TEST NAMED AFTER THE ALLOW-LIST THAT EXERCISES ONLY THE HALF WHICH
// DEFENDS NOTHING. TestUpdate_PatchesAllowlistedFields sends exactly one ALLOW-LISTED key
// and asserts only that no error came back. An allow-list has two halves — what it permits
// and what it REFUSES — and only the permitting half was written. Every other test in this
// package sends allow-listed keys too, so nothing anywhere fails when the gate is removed.
//
// This file adds the refusing half. It changes no behaviour: `updatable` is untouched and
// every assertion below already passes on main. What changes is that removing or widening
// the gate now reds.

// TestUpdate_RefusesKeysOutsideTheAllowlist is the negative half: an unlisted key must reach
// neither the SET clause nor the argument list.
func TestUpdate_RefusesKeysOutsideTheAllowlist(t *testing.T) {
	store, pool := newMockStore(t)

	// Exactly one allow-listed key survives, so the statement is fully determined: one
	// column, plus the updated_at the store always adds, plus the id. Matching the WHOLE
	// SET clause is the point — an assertion like "contains name" would pass while
	// `evil_col` sat next to it.
	pool.ExpectQuery(regexp.QuoteMeta(`UPDATE spaces SET name = $1, updated_at = $2 WHERE id = $3`)).
		WithArgs("renamed", pgxmock.AnyArg(), "s-1").
		WillReturnRows(spaceRow("s-1", "engineering"))

	if _, err := store.Update(context.Background(), "s-1", map[string]any{
		"name":       "renamed",  // allow-listed
		"evil_col":   "x",        // not a column this store may write
		"is_admin":   true,       // a privilege flag in the sibling package's map
		"created_by": "attacker", // a real column, deliberately NOT in `updatable`
	}); err != nil {
		t.Fatalf("Update with mixed keys: %v", err)
	}
	// newMockStore's cleanup calls ExpectationsWereMet, so a statement that carried the
	// extra columns fails here rather than passing quietly.
}

// TestUpdate_AllKeysUnlisted_IssuesNoUpdateAtAll is the stronger claim: when nothing the
// caller sent is writable, the store must not issue an UPDATE — it returns the row as-is.
// Only the SELECT is expected, so an UPDATE reaching the pool is an UNEXPECTED query and
// fails the test.
func TestUpdate_AllKeysUnlisted_IssuesNoUpdateAtAll(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(regexp.QuoteMeta(`SELECT`)).
		WithArgs("s-1").
		WillReturnRows(spaceRow("s-1", "engineering"))

	got, err := store.Update(context.Background(), "s-1", map[string]any{
		"evil_col": "x", "workspace_id": "other-ws", "slug": "stolen",
	})
	if err != nil {
		t.Fatalf("Update with only unlisted keys: %v", err)
	}
	if got == nil {
		t.Fatal("Update returned no space; the unlisted-key path must still return the row")
	}
	// ⚠ workspace_id and slug are REAL columns on this table and are deliberately absent
	// from `updatable`. If either is ever added there, this test reds — which is the
	// conversation that should happen, since workspace_id is the tenancy key.
}

// TestUpdate_IdentifierShapedKeyIsDropped makes the risk concrete rather than theoretical.
// The key is crafted to close the assignment and append a second one; it is not a column
// name and must never be interpolated.
func TestUpdate_IdentifierShapedKeyIsDropped(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(regexp.QuoteMeta(`SELECT`)).
		WithArgs("s-1").
		WillReturnRows(spaceRow("s-1", "engineering"))

	if _, err := store.Update(context.Background(), "s-1", map[string]any{
		`name = 'pwned', private`: true,
	}); err != nil {
		t.Fatalf("Update with an identifier-shaped key: %v", err)
	}
	// Reaching here with only the SELECT expected means the key never became SQL. With the
	// allow-list removed the store would issue
	//   UPDATE spaces SET name = 'pwned', private = $1, updated_at = $2 WHERE id = $3
	// and this test would fail on an unexpected query.
}

// TestUpdatableIsNotEmpty is the non-vacuity floor. Every assertion above is satisfied by an
// allow-list that permits NOTHING — the store would then never issue an UPDATE at all and
// the two SELECT-only tests would still pass. This keeps "refuses the wrong keys" from
// silently becoming "refuses every key".
func TestUpdatableIsNotEmpty(t *testing.T) {
	if len(updatable) < 5 {
		t.Fatalf("updatable has %d entries; this file was written against 5. An allow-list "+
			"that shrank to nothing satisfies every refusal assertion above while making the "+
			"PATCH route inert", len(updatable))
	}
	for _, must := range []string{"name", "description", "icon", "color", "private"} {
		if _, ok := updatable[must]; !ok {
			t.Errorf("%q is no longer updatable — a space can no longer be renamed/restyled "+
				"through PATCH. If that is deliberate, change this list in the same commit", must)
		}
	}
	// The other direction, and it is the one that matters: a column that must NEVER be
	// writable from a request body.
	for _, never := range []string{"id", "workspace_id", "created_by", "created_at", "slug"} {
		if _, ok := updatable[never]; ok {
			t.Errorf("%q is in `updatable`, so a PATCH body can write it. workspace_id is the "+
				"TENANCY key and created_by is the audit identity; neither may come from a "+
				"client. Adding one of these is a security change, not a feature toggle", never)
		}
	}
}
