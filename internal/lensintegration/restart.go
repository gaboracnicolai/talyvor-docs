package lensintegration

// restart.go — B17.88: a page translated, summarised or asked about while Lens restarts is
// answered once Lens is back, instead of "Couldn't translate this page — nothing was asked of the
// model".
//
// Docs calls Lens at http://lens:8080 on the compose network, and Lens is redeployed within minutes
// of every green merge. While its container is replaced, a dial to `lens` is refused or the name
// does not resolve, and every AI call Docs makes in that gap answered 502. The e2e run of
// 2026-10-06 saw one translate and one ask fail that way, each in under 2.5 s.
//
// ONLY A REQUEST WHOSE CONNECTION WAS NEVER OPENED IS SENT AGAIN. A dial error means not one byte
// reached Lens: nothing was asked of the model and nothing was billed, so sending it again charges
// the call once. Any error after the dial, and any answer Lens gives — a 502 included — is
// returned exactly as before, because Lens may already have priced it. (A POST on a reused
// connection that failed before anything was written is already replayed by net/http itself.)
//
// The wait is short on purpose: the BFF gives the whole exchange ten seconds, and the model still
// has to answer after Lens is back.

import (
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	// lensRestartWait is how long a call waits for a restarting Lens before it fails.
	lensRestartWait = 5 * time.Second
	// lensRestartRetryEvery is how often a refused dial is tried again.
	lensRestartRetryEvery = 250 * time.Millisecond
)

type restartTolerantTransport struct {
	base  http.RoundTripper
	wait  time.Duration
	every time.Duration
}

// RestartTolerant wraps base so a request Lens never received is sent again while Lens restarts.
// A nil base means http.DefaultTransport.
func RestartTolerant(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return restartTolerantTransport{base: base, wait: lensRestartWait, every: lensRestartRetryEvery}
}

func (t restartTolerantTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	giveUp := time.Now().Add(t.wait)
	for {
		resp, err := t.base.RoundTrip(req)
		if err == nil || !neverSent(err) || !replayable(req) || time.Now().After(giveUp) {
			return resp, err
		}
		timer := time.NewTimer(t.every)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
		if req.GetBody != nil {
			body, berr := req.GetBody()
			if berr != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}

// neverSent reports a failure to open the connection at all — refused, or `lens` not resolving
// while its container is replaced. Nothing of the request was written.
func neverSent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func replayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}
