package sharing_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/testutil"
)

// A share token works only as this server signed it. Every changed form of a live link's token is
// refused with the same 404 as an unknown link, and so is the nonce share_links stores, so the table
// alone does not hold a working link. The live token opening first is what makes each 404 a refusal
// rather than a link that never worked.
func TestSignedShareToken_TamperedTokenIsRefused_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	l := f.link(t, nil, "")

	if code, body := f.open(t, l.Token, ""); code != http.StatusOK {
		t.Fatalf("the untouched token did not open: %d %s", code, body)
	}

	nonce, sig, ok := strings.Cut(l.Token, ".")
	if !ok {
		t.Fatalf("token %q carries no signature", l.Token)
	}
	var stored string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT token FROM share_links WHERE id = $1`, l.ID).Scan(&stored); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	foreign, err := sharing.NewStore(d.Pool).WithSigningSecret("some-other-deployments-secret-0123").
		Create(context.Background(), f.pageID, f.ws, f.author, permission.AccessView, nil, "")
	if err != nil {
		t.Fatalf("mint a link under another secret: %v", err)
	}

	flip := func(s string, i int) string {
		c := byte('A')
		if s[i] == 'A' {
			c = 'B'
		}
		return s[:i] + string(c) + s[i+1:]
	}
	for name, tok := range map[string]string{
		"signature changed":        nonce + "." + flip(sig, len(sig)-1),
		"nonce changed":            flip(nonce, len(nonce)-1) + "." + sig,
		"signature removed":        nonce,
		"the stored nonce":         stored,
		"signed by another key":    foreign.Token,
		"signature from elsewhere": nonce + "." + strings.SplitN(foreign.Token, ".", 2)[1],
	} {
		if code, body := f.open(t, tok, ""); code != http.StatusNotFound {
			t.Errorf("%s: %q opened with %d %s, want 404", name, tok, code, body)
		}
	}
}
