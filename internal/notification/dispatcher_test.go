package notification

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/email"
	"github.com/talyvor/docs/internal/model"
)

// --- fakes ---

type fakeDir struct {
	pages    map[string]PageRef
	mentions map[string][]string // handle -> member ids
}

func (f *fakeDir) PageByID(_ context.Context, id string) (*PageRef, error) {
	if p, ok := f.pages[id]; ok {
		return &p, nil
	}
	return nil, errNotFound
}
func (f *fakeDir) ResolveMentions(_ context.Context, _ string, handles []string) ([]string, error) {
	var out []string
	for _, h := range handles {
		out = append(out, f.mentions[h]...)
	}
	return out, nil
}

type fakeRecipients struct{ emails map[string]Recipient }

func (f fakeRecipients) EmailsByIDs(_ context.Context, ids []string) (map[string]Recipient, error) {
	out := map[string]Recipient{}
	for _, id := range ids {
		if r, ok := f.emails[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

type fakePrefs struct{ optedOut map[string]bool }

func (f fakePrefs) EnabledMembers(_ context.Context, _ string, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		if !f.optedOut[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

type spyQueue struct{ msgs []email.Message }

func (s *spyQueue) Enqueue(m email.Message) bool { s.msgs = append(s.msgs, m); return true }
func (s *spyQueue) recipients() map[string]bool {
	out := map[string]bool{}
	for _, m := range s.msgs {
		for _, to := range m.To {
			out[to] = true
		}
	}
	return out
}

func rc(id, addr string) Recipient { return Recipient{MemberID: id, Email: addr, Name: id} }

func newTestDispatcher(t *testing.T, dir directory, rcpts recipientResolver, prefs prefChecker) (*Dispatcher, *spyQueue) {
	t.Helper()
	r, err := email.NewRenderer()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	q := &spyQueue{}
	d := newDispatcher(dir, rcpts, prefs, q, r, "https://docs.example.com", "Talyvor Docs", nil)
	return d, q
}

// --- tests ---

func TestDispatcher_ApprovalEmailsReviewersNotRequester(t *testing.T) {
	rcpts := fakeRecipients{emails: map[string]Recipient{
		"requester": rc("requester", "req@x.z"),
		"rev1":      rc("rev1", "rev1@x.z"),
		"rev2":      rc("rev2", "rev2@x.z"),
	}}
	dir := &fakeDir{pages: map[string]PageRef{"p1": {ID: "p1", SpaceID: "s1", Title: "Spec", WorkspaceID: "ws"}}}
	d, q := newTestDispatcher(t, dir, rcpts, fakePrefs{})

	d.ApprovalRequested(context.Background(), "p1", "ws", "requester", []string{"rev1", "rev2"}, "please review")

	got := q.recipients()
	if !got["rev1@x.z"] || !got["rev2@x.z"] {
		t.Fatalf("both reviewers should be emailed, got %v", got)
	}
	if got["req@x.z"] {
		t.Fatal("the requester (actor) must not be emailed")
	}
}

func TestDispatcher_ApprovalPreferenceOptOutSuppresses(t *testing.T) {
	rcpts := fakeRecipients{emails: map[string]Recipient{"rev1": rc("rev1", "rev1@x.z")}}
	dir := &fakeDir{pages: map[string]PageRef{"p1": {ID: "p1", Title: "Spec", WorkspaceID: "ws"}}}
	d, q := newTestDispatcher(t, dir, rcpts, fakePrefs{optedOut: map[string]bool{"rev1": true}})

	d.ApprovalRequested(context.Background(), "p1", "ws", "requester", []string{"rev1"}, "")
	if len(q.msgs) != 0 {
		t.Fatalf("opted-out reviewer must get nothing, got %d", len(q.msgs))
	}
}

func TestDispatcher_RecipientWithoutDirectoryRowIsSkipped(t *testing.T) {
	// rev1 has no recipient row → cannot be emailed; rev2 can.
	rcpts := fakeRecipients{emails: map[string]Recipient{"rev2": rc("rev2", "rev2@x.z")}}
	dir := &fakeDir{pages: map[string]PageRef{"p1": {ID: "p1", Title: "Spec", WorkspaceID: "ws"}}}
	d, q := newTestDispatcher(t, dir, rcpts, fakePrefs{})

	d.ApprovalRequested(context.Background(), "p1", "ws", "requester", []string{"rev1", "rev2"}, "")
	if len(q.msgs) != 1 || !q.recipients()["rev2@x.z"] {
		t.Fatalf("only rev2 (resolvable) should be emailed, got %v", q.recipients())
	}
}

func TestDispatcher_MentionEmailsResolvedUserNotActor(t *testing.T) {
	rcpts := fakeRecipients{emails: map[string]Recipient{
		"bob":    rc("bob", "bob@x.z"),
		"author": rc("author", "author@x.z"),
	}}
	dir := &fakeDir{
		pages:    map[string]PageRef{"p1": {ID: "p1", SpaceID: "s1", Title: "Spec", WorkspaceID: "ws"}},
		mentions: map[string][]string{"bob": {"bob"}, "author": {"author"}},
	}
	d, q := newTestDispatcher(t, dir, rcpts, fakePrefs{})

	d.PageMentioned(context.Background(), "p1", "hey @bob and @author look", "author")

	got := q.recipients()
	if !got["bob@x.z"] {
		t.Fatal("mentioned user bob should be emailed")
	}
	if got["author@x.z"] {
		t.Fatal("the author (actor) must not be emailed even if they @-mention themselves")
	}
}

func TestDispatcher_StaleDigestOneEmailPerOwnerWithTheirPages(t *testing.T) {
	rcpts := fakeRecipients{emails: map[string]Recipient{
		"alice": rc("alice", "alice@x.z"),
		"bob":   rc("bob", "bob@x.z"),
	}}
	d, q := newTestDispatcher(t, &fakeDir{}, rcpts, fakePrefs{})

	stale := []model.Page{
		{ID: "p1", Title: "Alice A", SpaceID: "s1", CreatedBy: "alice", StaleAfterDays: 30},
		{ID: "p2", Title: "Alice B", SpaceID: "s1", CreatedBy: "alice", StaleAfterDays: 30},
		{ID: "p3", Title: "Bob A", SpaceID: "s1", CreatedBy: "bob", StaleAfterDays: 30},
		{ID: "p4", Title: "Ghost", SpaceID: "s1", CreatedBy: "nobody", StaleAfterDays: 30}, // no recipient row
	}
	d.StaleDigest(context.Background(), "ws", stale)

	// Exactly two emails: one to alice, one to bob. "nobody" is skipped.
	if len(q.msgs) != 2 {
		t.Fatalf("want 2 digest emails (alice, bob), got %d", len(q.msgs))
	}
	var aliceMsg *email.Message
	for i := range q.msgs {
		if q.msgs[i].To[0] == "alice@x.z" {
			aliceMsg = &q.msgs[i]
		}
	}
	if aliceMsg == nil {
		t.Fatal("alice should get a digest")
	}
	// Alice's digest lists both her pages and neither of the others.
	if !contains(aliceMsg.HTMLBody, "Alice A") || !contains(aliceMsg.HTMLBody, "Alice B") {
		t.Error("alice's digest should list both her stale pages")
	}
	if contains(aliceMsg.HTMLBody, "Bob A") {
		t.Error("alice's digest must not include bob's page")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
