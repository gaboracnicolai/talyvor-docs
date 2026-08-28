package mountguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/gatewayauth"
)

// THE AUTH EXEMPTIONS, AS A POPULATION. W3.31 pinned the same shape one layer down, where it
// costs memory; this is the layer where it costs the boundary.
//
// cmd/docs guards /v1 with two middlewares, each taking an exempt PREDICATE:
//
//	r.Use(gatewayauth.Middleware(cfg.GatewayAuthSecret, gatewayauth.ExemptTransitProof))
//	r.Use(authz.Middleware(authzResolver, gatewayauth.ExemptMembership))
//
// The predicates are shared definitions (internal/gatewayauth/exempt.go) and several tests drive
// the REAL chain through them — which is why the routes that ARE exempt behave correctly. What
// nothing asked is the population question: **WHICH routes does each predicate exempt, and would
// anything notice if a new one joined?**
//
// ⚠⚠ MEASURED 2026-08-28 (tab-c5j7, W3.32), by evaluating the real predicates against the real
// route table rather than by reading them: of 103 registered /v1 routes, **exactly ONE is exempt
// from the gateway secret and exactly ONE more is exempt from identity** — the two the exempt.go
// header describes. Nothing was unintended. **AND NOTHING PINNED IT.** Adding a route under
// /v1/public/ ships an endpoint reachable with NO gateway secret; adding one under /v1/service/
// ships an endpoint that skips membership resolution. Either is a one-line diff in a handler's
// Mount() — a file that need not mention auth at all — and no test in this repository would have
// changed colour.
//
// ⚠ THIS GUARD ADDS NO PERMISSION AND REMOVES NONE. Every route below is exempt today and stays
// exempt; the predicates are untouched. It records the population so that CHANGING it is a
// deliberate edit to a named table rather than a silent consequence of choosing a path. That is
// the posture W3.29/W3.30/W3.31 held about numbers and wiring, applied to the boundary — and on
// an authz path it is the ONLY posture a session may take.
//
// ⚠ THE PREDICATES ARE CALLED, NOT RE-IMPLEMENTED. A census that re-declared "/v1/public/" would
// pass forever after exempt.go changed — testing a boundary that no longer exists, which is the
// specific failure exempt.go's own header says the shared definition exists to prevent.
//
// Controls: ~/talyvor-queue/w332-authexempt-controls-c5j7.py.

// exemptFromGatewaySecret: reachable with NO x-gateway-auth secret at all.
// The share viewer authenticates with its own share token, carried in the path.
var exemptFromGatewaySecret = []string{
	"sharing GET /v1/public/s/{token}",
}

// exemptFromIdentityOnly: the secret is still required; only the VERIFIED IDENTITY is skipped.
// exempt.go: the caller is a server holding the shared secret, reconciling membership for a
// workspace whose membership has not been read yet — so there is no membership to authorize
// against, by construction.
var exemptFromIdentityOnly = []string{
	"trackintegration POST /v1/service/workspaces/{wsID}/member-sync",
}

// v1MountPrefix reads the prefix cmd/docs mounts the guarded group under, rather than assuming
// "/v1": collect() reports paths relative to that mount, so a wrong prefix here would evaluate
// the predicates against paths that can never match and report a clean, empty population.
func v1MountPrefix(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "cmd", "docs", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var prefix string
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		defer func() { stack = append(stack, n) }()
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Middleware" {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || base.Name != "gatewayauth" {
			return true
		}
		// only the site that passes the SHARED predicate, not the /mcp group's local one
		if q, ok := call.Args[1].(*ast.SelectorExpr); !ok || q.Sel.Name != "ExemptTransitProof" {
			return true
		}
		for i := len(stack) - 1; i >= 0; i-- {
			c, ok := stack[i].(*ast.CallExpr)
			if !ok || len(c.Args) != 2 {
				continue
			}
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Route" {
				if b, ok := c.Args[0].(*ast.BasicLit); ok && b.Kind == token.STRING {
					if v, err := strconv.Unquote(b.Value); err == nil {
						prefix = v
						return false
					}
				}
			}
		}
		return true
	})
	if prefix == "" {
		t.Fatalf("could not find the mount prefix of the group that uses "+
			"gatewayauth.ExemptTransitProof in %s. A census that cannot locate its own subject "+
			"must fail LOUDLY — with an empty prefix every predicate below would be evaluated "+
			"against a path that cannot match and the population would read as empty, which is "+
			"indistinguishable from a boundary with nothing exempt.", path)
	}
	return prefix
}

// TestAuthExemptions_ArePinnedAsAPopulation fails when a route joins or leaves either exemption.
func TestAuthExemptions_ArePinnedAsAPopulation(t *testing.T) {
	mount := v1MountPrefix(t)
	routes, _ := collect(t)
	if len(routes) == 0 {
		t.Fatal("collect() returned no routes — a census with no subject agrees with any table")
	}

	var noSecret, noIdentity []string
	for _, r := range routes {
		full := mount + r.path
		id := r.pkg + " " + r.method + " " + full
		switch {
		case gatewayauth.ExemptTransitProof(full):
			noSecret = append(noSecret, id)
		case gatewayauth.ExemptMembership(full):
			noIdentity = append(noIdentity, id)
		}
	}
	sort.Strings(noSecret)
	sort.Strings(noIdentity)

	check := func(label string, got, want []string, cost string) {
		w := append([]string(nil), want...)
		sort.Strings(w)
		if strings.Join(got, "\n") == strings.Join(w, "\n") {
			return
		}
		t.Errorf("the set of routes %s CHANGED.\n  recorded: %v\n  found:    %v\n%s\n"+
			"This guard adds and removes no permission — it records the boundary as it ships. "+
			"If the change is intended, update the table in the SAME commit that changes the "+
			"route, so the boundary moves visibly instead of as a side effect of a path.",
			label, w, got, cost)
	}
	check("exempt from the GATEWAY SECRET", noSecret, exemptFromGatewaySecret,
		"  A route here is reachable with NO x-gateway-auth secret. exempt.go: the secret is the "+
			"only thing between these paths and the internet.")
	check("exempt from IDENTITY only", noIdentity, exemptFromIdentityOnly,
		"  A route here still needs the secret but skips membership resolution, so handlers get "+
			"no verified identity to scope by.")

	// ── vacuity floor, stated per lane. Zero is not "clean", it is "the predicate matched
	// nothing" — which is exactly what a wrong mount prefix looks like.
	if len(noSecret)+len(noIdentity) == 0 {
		t.Fatalf("no route matched EITHER exemption under mount %q, yet cmd/docs still passes both "+
			"predicates to its middlewares. Either this census stopped resolving paths, or the "+
			"exempted routes are gone and the EXEMPTIONS should go with them.", mount)
	}
	t.Logf("mount %q · %d route exempt from the gateway secret · %d exempt from identity only "+
		"(of %d registered)", mount, len(noSecret), len(noIdentity), len(routes))
}
