package trackintegration

import (
	"context"
	"errors"
	"testing"
)

// enumerate_test.go — the cold-start deadlock, and the fallback that must not reintroduce it.
//
// THE DEADLOCK. SyncMembers enumerated workspaces from Docs' OWN content
// (`SELECT workspace_id FROM spaces UNION SELECT workspace_id FROM pages`). A workspace with no
// content is never enumerated → never gets a roster → every write 403s for want of a membership
// row → it never gets content. A brand-new tenant could not create their first page, ever.
//
// THE FIX is to invert the source: ask Track, which is the tenancy source of truth and mints a
// workspace per identity at login, instead of asking Docs whether the workspace has already been
// used. Content-derived enumeration answers "has this workspace been written to", which is a
// different question from "does this workspace exist" — and the gap between them is the deadlock.
//
// ⚠ THE FALLBACK IS THE DANGEROUS PART. Falling back to content-derived on a Track outage keeps
// existing rosters fresh, but it must not be able to DELETE anything: a fallback that returns a
// smaller set than reality is indistinguishable, downstream, from "those workspaces went away".
// The per-workspace guard in SyncMembers already refuses to prune on a failed pull; these tests
// pin that the enumeration change does not route around it.

type stubEnumSource struct {
	ids    []string
	err    error
	called int
}

func (s *stubEnumSource) ListWorkspaceIDs(context.Context) ([]string, error) {
	s.called++
	return s.ids, s.err
}

type stubStoreSource struct {
	ids    []string
	err    error
	called int
}

func (s *stubStoreSource) DistinctWorkspaceIDs(context.Context) ([]string, error) {
	s.called++
	return s.ids, s.err
}

// THE CLAIM THAT MATTERS: a workspace Track knows about but Docs has no content for is enumerated.
// That is exactly the brand-new tenant, and exactly what 403s today.
func TestEnumerate_IncludesAWorkspaceWithNoContent(t *testing.T) {
	track := &stubEnumSource{ids: []string{"ws-brand-new", "ws-existing"}}
	store := &stubStoreSource{ids: []string{"ws-existing"}} // content-derived sees only the used one

	got, err := enumerateWorkspaces(context.Background(), track, store)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if !hasID(got, "ws-brand-new") {
		t.Errorf("enumeration = %v — a workspace with no content was omitted, which is the "+
			"cold-start deadlock: it never gets a roster, so its first write 403s forever", got)
	}
	if store.called != 0 {
		t.Errorf("content-derived enumeration ran even though Track answered; Track is the source " +
			"of truth and must not be second-guessed on the happy path")
	}
}

// Track unreachable ⇒ fall back to content-derived, so a Track blip cannot stop existing rosters
// being refreshed.
func TestEnumerate_FallsBackWhenTrackIsUnreachable(t *testing.T) {
	track := &stubEnumSource{err: errors.New("connection refused")}
	store := &stubStoreSource{ids: []string{"ws-existing"}}

	got, err := enumerateWorkspaces(context.Background(), track, store)
	if err != nil {
		t.Fatalf("fallback should not error: %v", err)
	}
	if !hasID(got, "ws-existing") {
		t.Errorf("enumeration = %v, want the content-derived set when Track is down", got)
	}
	if store.called != 1 {
		t.Errorf("fallback did not consult the content-derived source (called %d times)", store.called)
	}
}

// ⚠ Both sources failing must yield an ERROR, not an empty list. An empty list would be handed to
// a loop that reconciles each workspace — and "no workspaces" is silently the same shape as "no
// work to do", so a total outage would look like a clean run forever.
func TestEnumerate_BothSourcesFailingIsAnErrorNotEmptiness(t *testing.T) {
	track := &stubEnumSource{err: errors.New("connection refused")}
	store := &stubStoreSource{err: errors.New("db down")}

	got, err := enumerateWorkspaces(context.Background(), track, store)
	if err == nil {
		t.Fatalf("both sources failed but enumerate returned %v and no error — a total outage must "+
			"not be reported as an empty, successful enumeration", got)
	}
	if len(got) != 0 {
		t.Errorf("enumerate returned %v alongside an error; callers must get nothing to act on", got)
	}
}

// Track answering with an EMPTY list is a real answer (a deployment with no workspaces yet), not a
// failure — but it must not be mistaken for one and trigger the fallback, because the fallback's
// content-derived set could then resurrect workspaces Track no longer knows about.
func TestEnumerate_TrackEmptyIsAnAnswerNotAFailure(t *testing.T) {
	track := &stubEnumSource{ids: []string{}}
	store := &stubStoreSource{ids: []string{"ws-stale"}}

	got, err := enumerateWorkspaces(context.Background(), track, store)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("enumeration = %v, want empty — Track returned an empty list, which is an answer", got)
	}
	if store.called != 0 {
		t.Errorf("an empty-but-successful Track response triggered the fallback")
	}
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
