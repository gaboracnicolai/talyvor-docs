package mcp_test

// THE ask_docs TOOL COULD NOT FAIL. IT ANSWERED UNGROUNDED AND CALLED IT SUCCESS.
//
// toolAskDocs discarded BOTH errors on its path:
//
//	hits, _ := s.deps.pages.SearchWithRank(...)          // server.go:800 — search failure dropped
//	ans, err := s.deps.ai.AskDocs(...); if err == nil {} // server.go:815 — AI failure dropped
//
// Its own comment said it "uses the same approach the REST /ask endpoint uses". The REST
// sibling (internal/ai/handler.go:223-243) returns HTTP 500 "search failed" on the first and
// writeAIErr — 503 AI_UNAVAILABLE / 502 AI_FAILED — on the second. The approach was not the
// same in the only respect that matters when something breaks.
//
// ⚠ MEASURED ON THE SHIPPED CHAIN BEFORE THIS GUARD EXISTED, at `198636a`, with NO fakes below
// the tool: real Postgres, real page.Store, real ai.Engine, real lensintegration.Client, real
// lenscreds provider, through POST /mcp with the production gatewayauth+authz middleware. The
// search fault is a page.Store on a database whose migrations were never applied, so Postgres
// itself rejects the real query (SQLSTATE 42P01); the AI fault is a real Lens answering 500.
//
//	(1) search ERRORS      -> HTTP 200 | {"answer":"Run make deploy.","sources":[]}
//	(2) search EMPTY (fine) -> HTTP 200 | {"answer":"Run make deploy.","sources":[]}
//	(1) == (2), BYTE-IDENTICAL
//	(3) AI ERRORS          -> HTTP 200 | {"answer":"","sources":[{"title":"Deploy guide",...}]}
//	(4) all healthy        -> HTTP 200 | {"answer":"Run make deploy.","sources":[{...}]}
//
// (1) IS THE WHOLE FINDING AND IT IS WORSE THAN A DROPPED ERROR. The corpus was never read —
// Postgres refused the query outright — and the model still answered, from zero context pages,
// under a system prompt that tells it to use ONLY the provided documentation. The AI agent
// consuming this tool receives a confident answer that is byte-for-byte what it receives when
// the documentation genuinely has nothing to say. Neither the agent nor the human behind it can
// tell "your docs do not cover this" from "the search index is down".
//
// (3) IS THE SAME SHAPE ONE STEP LATER: the AI call failed and the tool reports the answer as
// the empty string WITH sources attached — which reads as "I read Deploy guide and it says
// nothing", a claim about a document, rather than "I could not ask".
//
// WHAT THIS ASSERTS IS A SET BOTH DIRECTIONS, because "return an error" is satisfiable by a
// change that errors on everything. A failed search and a failed AI must FAIL; a search that
// legitimately matched nothing, and a fully healthy call, must still SUCCEED.
//
// ⚠ AND A THIRD STATE THAT I FIRST WROTE DOWN WRONG, CORRECTED BY RUNNING THE PACKAGE RATHER
// THAN BY READING IT. Passing NO engine (ai == nil) also measured {"answer":"","sources":[...]}
// pre-fix, and I recorded that as "the `if s.deps.ai != nil` branch short-circuits, so nil is a
// separate state I am deliberately not touching". IT IS NOT A SEPARATE STATE AND THE nil CHECK
// DOES NOT SHORT-CIRCUIT. deps.ai is an aiDeps INTERFACE and New takes a *ai.Engine, so a nil
// engine becomes a NON-NIL interface holding a nil pointer — the footgun ai.Engine.IsAvailable
// documents in its own header. The call proceeds on the nil receiver, IsAvailable is
// nil-receiver-safe and returns false, run() returns ai.ErrUnavailable — and the tool swallowed
// THAT. So all three of "Lens is down", "Lens is unconfigured" and "no engine at all" were one
// empty string, and after this change all three are errors. What proved it was not the source:
// it was `sec_ratelimit_test.go` going red with err="ai: lens unavailable" from a server
// constructed with a nil engine.
//
// ⚠ WHICH MADE THAT TEST'S POSITIVE HALF VACUOUS, and fixing it is part of this merge:
// mcpLimitChain wired ai=nil and asserted the two in-burst ask_docs calls returned `code == 0`.
// They did — from a call that never reached Lens at all. A totally dead AI satisfied "the burst
// is available". It now wires a working engine, so success means success.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// askChain mounts one server behind the SAME middleware main.go uses, so a case reaches
// dispatch exactly as a real MCP client does. The authz resolver always reads the MIGRATED
// database — only the page store under test is swapped, so a case never fails at the door for
// the reason it is trying to measure past.
func askChain(t *testing.T, d *testutil.DB, srv *mcp.Server) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	return r
}

// askLens is a real HTTP Lens. It serves the token-mint endpoint (a completion mints a
// per-workspace bearer before it sends anything), then answers. answer=="" means Lens is DOWN:
// it replies 500, which is a transport failure the real client turns into a real error.
func askLens(t *testing.T, answer string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`,
				time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		if answer == "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"lens is down"}`))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, answer)))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func askEngine(t *testing.T, answer string) *ai.Engine {
	t.Helper()
	u := askLens(t, answer)
	return ai.New(lensintegration.New(u, "k1").WithTokenProvider(lenscreds.New(u, "k1", lenscreds.Options{})))
}

// askResult is what an MCP client actually sees: whether the envelope carried an `error`, and
// the tool payload when it did not.
type askResult struct {
	rpcErr  bool
	errText string
	payload string
}

func askDocs(t *testing.T, d *testutil.DB, srv *mcp.Server, ws, question string) askResult {
	t.Helper()
	rr := callTool(askChain(t, d, srv), "bob@corp.com", true, "ask_docs",
		map[string]any{"question": question, "workspace_id": ws})
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v — %s", err, rr.Body.String())
	}
	if len(env.Error) > 0 {
		return askResult{rpcErr: true, errText: string(env.Error)}
	}
	if len(env.Result.Content) == 0 {
		t.Fatalf("neither error nor content in the response: %s", rr.Body.String())
	}
	return askResult{payload: env.Result.Content[0].Text}
}

func TestMCPAskDocsReportsFailureRatherThanAnsweringUngrounded_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	bob := d.Member(t, ws, "bob@corp.com")
	_ = d.Page(t, ws, bob, "Deploy guide")

	// A database that is created and connectable but was NEVER migrated: `pages` does not
	// exist, so page.Store's real SQL is rejected by Postgres. This is a genuine store
	// failure, not a stub returning a manufactured error.
	blank := testutil.NewBlank(t)
	brokenStore := page.NewStore(blank.Pool)
	healthyStore := page.NewStore(d.Pool)

	// ── PREMISES. Every fault is proven to fire and the healthy engine proven to answer
	// BEFORE any case below is read as evidence. A control whose fault never fired reports
	// a working guard as broken, and a "healthy" comparator that is quietly also broken
	// makes the discrimination cases vacuous.
	if _, err := brokenStore.SearchWithRank(t.Context(), ws, "deploy", nil, 3, 0); err == nil {
		t.Fatal("PREMISE: the unmigrated store did not error — the search fault never fired, " +
			"so every search case below would be measuring nothing")
	}
	if _, err := healthyStore.SearchWithRank(t.Context(), ws, "deploy", nil, 3, 0); err != nil {
		t.Fatalf("PREMISE: the migrated store errored (%v) — the must-succeed cases would pass "+
			"for the wrong reason", err)
	}
	if _, err := askEngine(t, "").AskDocs(t.Context(), ws, "q", nil); err == nil {
		t.Fatal("PREMISE: the 500 Lens did not error — the AI fault never fired")
	}
	if a, err := askEngine(t, "Run make deploy.").AskDocs(t.Context(), ws, "q", nil); err != nil || a == "" {
		t.Fatalf("PREMISE: the healthy engine did not answer (%q, %v)", a, err)
	}

	newSrv := func(pages *page.Store, engine *ai.Engine) *mcp.Server {
		return mcp.New(pages, space.NewStore(d.Pool), nil, engine, nil, "test")
	}
	good := askEngine(t, "Run make deploy.")

	cases := []struct {
		name     string
		srv      *mcp.Server
		question string
		wantErr  bool
		why      string
	}{
		{
			name: "search fails", srv: newSrv(brokenStore, good),
			question: "how do I deploy?", wantErr: true,
			why: "Postgres refused the search query and the tool answered anyway, from zero " +
				"context pages, indistinguishably from a corpus that had nothing to say",
		},
		{
			name: "search legitimately matches nothing", srv: newSrv(healthyStore, good),
			question: "zzzzq-no-such-word-anywhere-zzzzq", wantErr: false,
			why: "an empty result set is a fine answer, not a failure — erroring here would be " +
				"an over-block, and it is the half that keeps the case above from being " +
				"satisfiable by erroring on everything",
		},
		{
			name: "ai fails", srv: newSrv(healthyStore, askEngine(t, "")), // Lens answers 500
			question: "deploy", wantErr: true,
			why: "the AI call failed and the tool reported the answer as the empty string with " +
				"sources attached — a claim about the documents, not a report of a failure",
		},
		{
			name: "everything healthy", srv: newSrv(healthyStore, good),
			question: "deploy", wantErr: false,
			why: "the working path must still work",
		},
	}

	var searchFailed, searchEmpty string
	for _, c := range cases {
		got := askDocs(t, d, c.srv, ws, c.question)
		if got.rpcErr != c.wantErr {
			t.Errorf("%s: rpcError=%v want %v — %s\n  error=%s\n  payload=%s",
				c.name, got.rpcErr, c.wantErr, c.why, got.errText, got.payload)
		}
		switch c.name {
		case "search fails":
			searchFailed = got.payload
		case "search legitimately matches nothing":
			searchEmpty = got.payload
		case "everything healthy":
			if !got.rpcErr && !strings.Contains(got.payload, "make deploy") {
				t.Errorf("healthy call lost its answer: %s", got.payload)
			}
			if !got.rpcErr && !strings.Contains(got.payload, "Deploy guide") {
				t.Errorf("healthy call lost its sources: %s", got.payload)
			}
		}
	}

	// THE PROPERTY, NAMED SEPARATELY FROM THE MECHANISM. Even if both cases above somehow
	// returned a payload, a broken index must never be reportable as an empty corpus.
	if searchFailed != "" && searchFailed == searchEmpty {
		t.Errorf("a FAILED search and an EMPTY search are byte-identical to the caller:\n  %s\n"+
			"an agent cannot tell 'your docs do not cover this' from 'the search index is down'",
			searchFailed)
	}

	// AND THE ERROR MUST NOT CARRY THE DATABASE'S OWN WORDS to an MCP client. The REST
	// sibling logs the cause and returns a fixed message; this asserts the same discipline
	// rather than trusting it.
	leaky := askDocs(t, d, newSrv(brokenStore, good), ws, "how do I deploy?")
	for _, forbidden := range []string{"SQLSTATE", "relation", "does not exist", "pgx"} {
		if strings.Contains(leaky.errText, forbidden) {
			t.Errorf("the rpc error leaks the store's internals (%q): %s", forbidden, leaky.errText)
		}
	}
}
