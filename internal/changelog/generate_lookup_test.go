package changelog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/docs/internal/trackintegration"
)

// ─── The generated changelog's issue lookup, driven through the REAL Track client ───
//
// ⚠ WHY THESE TESTS EXIST AND `TestGenerateFromIssues_BuildContentGroupsHeadings` COULD NOT
// STAND IN FOR THEM. That test proves the grouping works — against `fakeTrack`, whose
// GetIssue is `func (f *fakeTrack) GetIssue(_ context.Context, _, id string)`. It DISCARDS
// the context and the workspace id and keys on the issue id alone. Those are precisely the
// two arguments production got wrong, so the fixture answers a lookup the real client
// refuses, and no assertion over it can see the difference. A stub that ignores an argument
// cannot fail for a caller that passes the wrong one.
//
// The subject here is therefore the REAL `*trackintegration.Client` against an httptest
// Track. It is the smallest fixture in which the two lookups are distinguishable.
//
// ⚠ AND THE FAILURE IS SILENT BY CONSTRUCTION, WHICH IS WHY IT NEEDED A TEST AND NOT A READ:
// `Client.GetIssue` deliberately swallows every error into `(nil, nil)` (its own comment says
// the swallow is right for a rendered embed), and `buildContent` reads it as `ref, _ :=`.
// Two independent silencers in series — the call cannot report that it failed, and the caller
// would not look.

// fakeTrackServer is a real HTTP Track. It records every path it is asked for, so a test can
// assert on what the client ACTUALLY requested rather than on what it returned.
type fakeTrackServer struct {
	mu     sync.Mutex
	paths  []string
	issues map[string]string // issueID → JSON body
}

func newFakeTrackServer(t *testing.T, issues map[string]string) (*trackintegration.Client, *fakeTrackServer) {
	t.Helper()
	f := &fakeTrackServer{issues: issues}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		// Only answer the fully-qualified shape Track really serves:
		// /v1/workspaces/{wsID}/issues/{issueID}, with a NON-EMPTY workspace id.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 || parts[0] != "v1" || parts[1] != "workspaces" || parts[3] != "issues" || parts[2] == "" {
			http.NotFound(w, r)
			return
		}
		body, ok := f.issues[parts[4]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return trackintegration.New(srv.URL, "test-key"), f
}

func (f *fakeTrackServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

func threeLabelledIssues() map[string]string {
	return map[string]string{
		"i-1": `{"id":"i-1","identifier":"ENG-1","title":"Auth bug","labels":["bug"]}`,
		"i-2": `{"id":"i-2","identifier":"ENG-2","title":"Dark mode","labels":["feature"]}`,
		"i-3": `{"id":"i-3","identifier":"ENG-3","title":"Drop v1 API","labels":["breaking-change"]}`,
	}
}

// TestBuildContent_ResolvesIssuesThroughTheRealClient is the guard on the defect itself.
//
// buildContent used to call `track.GetIssue(nil, "", id)` — a nil context and an empty
// workspace id — so `http.NewRequestWithContext` failed before a request was ever built and
// every issue degraded to its bare id in the improvement bucket.
func TestBuildContent_ResolvesIssuesThroughTheRealClient(t *testing.T) {
	client, srv := newFakeTrackServer(t, threeLabelledIssues())

	content := buildContent(context.Background(), client, "ws-1", []string{"i-1", "i-2", "i-3"})

	// The titles and identifiers Track served must reach the document.
	for _, want := range []string{"ENG-1", "Auth bug", "ENG-2", "Dark mode", "ENG-3", "Drop v1 API"} {
		if !strings.Contains(content, want) {
			t.Errorf("[LOOKUP] generated changelog is missing %q — the issue was not resolved: %s", want, content)
		}
	}
	// The label-derived buckets must be the ones Track's labels imply, not the
	// everything-fell-through default.
	for _, want := range []string{"Bug Fixes", "New Features", "Breaking Changes"} {
		if !strings.Contains(content, want) {
			t.Errorf("[BUCKET] generated changelog is missing the %q heading: %s", want, content)
		}
	}
	if strings.Contains(content, "Improvements") {
		t.Errorf("[BUCKET] every issue fell through to the improvement bucket — the lookup returned nil for all of them: %s", content)
	}
	if n := srv.requestCount(); n == 0 {
		t.Errorf("[REQUEST] the Track client never issued a request at all (want 3); " +
			"a nil context fails inside http.NewRequestWithContext, before the wire")
	}
}

// TestBuildContent_AsksTrackForTheCallersWorkspace pins the workspace id specifically.
//
// ⚠ THIS IS A SEPARATE ASSERTION FROM THE ONE ABOVE ON PURPOSE. A context alone would make
// the request happen; it would still be built against `/v1/workspaces//issues/{id}`, which is
// a DIFFERENT tenant's-worth of wrong — an unscoped read that no gate has authorized. The
// test above passes with a correct ctx and an empty workspace only because this server 404s
// that shape; this one says why that 404 is the right answer rather than an accident.
func TestBuildContent_AsksTrackForTheCallersWorkspace(t *testing.T) {
	client, srv := newFakeTrackServer(t, threeLabelledIssues())

	buildContent(context.Background(), client, "ws-1", []string{"i-1"})

	srv.mu.Lock()
	paths := append([]string(nil), srv.paths...)
	srv.mu.Unlock()

	if len(paths) == 0 {
		t.Fatal("[WS] no request reached Track at all")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/v1/workspaces/ws-1/issues/") {
			t.Errorf("[WS] Track was asked for %q — the caller's workspace id did not reach the lookup", p)
		}
	}
}

// TestGenerateFromIssues_BadgeAndBodyAgree is the finding's user-visible half.
//
// ⚠ THE ENTRY'S TYPE BADGE WAS COMPUTED FROM A LOOKUP THE BODY DID NOT GET. GenerateFromIssues
// resolves each issue a SECOND time, with the correct ctx and workspace, purely to pick the
// dominant bucket — so the badge read `breaking` over a body in which every issue had fallen
// through to `Improvements` as a bare id. The entry looked authoritative and its content was
// the degraded one. Two lookups of the same fact in one call tree, one right and one wrong, is
// the shape this asserts against: whatever the badge claims, the body must contain.
// ⚠⚠ THE TWO CLIENTS ARE NOT A STYLE CHOICE — WITH ONE, THIS GUARD COULD NOT FAIL.
// Control C4 regressed dominantType's own lookup to a nil context and this test STAYED GREEN.
// `Client.fetchIssue` consults its cache (client.go:133) BEFORE it builds a request, so the
// buildContent pass above warms (ws-1|i-*) and every later lookup is answered without ever
// touching the context. The badge half was therefore asserted over a cache hit, not over a
// lookup. A fresh client per half is the only shape in which each one's own arguments are
// still load-bearing. Found by the control, not by reading — C4 is the reason this line exists.
func TestGenerateFromIssues_BadgeAndBodyAgree(t *testing.T) {
	bodyClient, _ := newFakeTrackServer(t, threeLabelledIssues())
	badgeClient, _ := newFakeTrackServer(t, threeLabelledIssues())

	ids := []string{"i-1", "i-2", "i-3"}
	content := buildContent(context.Background(), bodyClient, "ws-1", ids)
	badge := dominantType(context.Background(), badgeClient, "ws-1", ids)

	if badge != EntryBreaking {
		t.Fatalf("[BADGE] dominant type = %q, want %q — the badge's own lookup regressed", badge, EntryBreaking)
	}
	if !strings.Contains(content, groupTitles[badge]) {
		t.Errorf("[AGREE] the entry is badged %q but its body has no %q section: %s",
			badge, groupTitles[badge], content)
	}
}

// TestBuildContent_TrackUnconfiguredStillDegrades is the must-stay-green companion.
//
// It keeps the guards above from being a catch-all: the graceful path — Track not configured
// at all — must still produce bare ids under Improvements and must NOT start erroring. If a
// "fix" made the lookup mandatory this reds, and that is the point.
func TestBuildContent_TrackUnconfiguredStillDegrades(t *testing.T) {
	unconfigured := trackintegration.New("", "")

	content := buildContent(context.Background(), unconfigured, "ws-1", []string{"i-1", "i-2"})

	if !strings.Contains(content, "Improvements") {
		t.Errorf("[DEGRADE] unconfigured Track should still bucket to Improvements: %s", content)
	}
	for _, want := range []string{"i-1", "i-2"} {
		if !strings.Contains(content, want) {
			t.Errorf("[DEGRADE] unconfigured Track should still list the bare id %q: %s", want, content)
		}
	}
}
