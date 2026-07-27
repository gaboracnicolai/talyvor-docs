package gatewayauth

import "strings"

// The /v1 exemption predicates. THERE ARE TWO, AND THEY DIFFER ON PURPOSE.
//
// Docs' /v1 surface has two lanes with different callers and different proofs:
//
//	USER lane    (/v1/spaces, /v1/pages, /v1/workspaces/{id}/…)
//	  transit proof + a verified identity resolved to workspace memberships.
//	  The membership set is the tenancy boundary; every by-id query scopes to it.
//
//	SERVICE lane (/v1/service/…)
//	  transit proof ONLY. The caller is another server holding the shared secret —
//	  the BFF — and it has no user. Requiring an identity here is not stricter, it
//	  is incoherent: the one route in this lane exists to reconcile membership for
//	  a workspace whose membership has not been read yet, so there is by
//	  construction no membership to authorize against.
//
//	PUBLIC lane  (/v1/public/…)
//	  neither. The share viewer authenticates with its own share token.
//
// ⚠ WHY THIS IS A SHARED DEFINITION RATHER THAN A LITERAL IN main.go. A test that
// re-declares the predicate is testing a router that may not exist: the service
// route shipped green because every test called the handler's function directly,
// and the one thing nobody exercised was the middleware above it. Exporting the
// predicates means a test builds the REAL chain, and changing the boundary changes
// it for the tests in the same edit.

// ExemptTransitProof lists paths that need NO gateway secret. Public share viewing
// only — it carries its own token.
//
// ⚠ THE SERVICE LANE IS DELIBERATELY ABSENT. The secret is the ONLY thing standing
// between /v1/service/ and the internet, since those routes skip membership. If a
// service route ever appears here, it is unauthenticated.
func ExemptTransitProof(path string) bool {
	return strings.HasPrefix(path, "/v1/public/")
}

// ExemptMembership lists paths that need no VERIFIED IDENTITY — the public lane,
// plus the service lane, whose caller is a server rather than a person.
//
// Exempting a prefix is a standing decision, so state what keeps it safe: every
// path under /v1/service/ is still behind ExemptTransitProof's secret check, which
// runs first and refuses anything without it. The exemption moves the boundary from
// "who are you" to "prove you are us"; it does not remove one.
func ExemptMembership(path string) bool {
	return ExemptTransitProof(path) || strings.HasPrefix(path, "/v1/service/")
}
