package editsession

import "context"

// EditGuard is the save-path guard contract — structurally identical to page.editGuard and
// collab.LockGuard. Both the manual pagelock and this package's Store satisfy it.
type EditGuard interface {
	CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error)
}

// Composite ANDs several guards: a save is permitted only if EVERY guard permits. The first to
// deny (or error) wins, and its reason is returned. This is how the store guard becomes
// "approvalOK AND manualLockOK AND editSessionOK" WITHOUT any guard being replaced — each
// guard keeps its own behavior; Composite only conjoins them.
type Composite struct{ Guards []EditGuard }

// Compose builds a Composite from the given guards, skipping nils so callers can pass optional
// guards without branching.
func Compose(guards ...EditGuard) Composite {
	out := make([]EditGuard, 0, len(guards))
	for _, g := range guards {
		if g != nil {
			out = append(out, g)
		}
	}
	return Composite{Guards: out}
}

// CanEdit returns allow only if all composed guards allow. On the first denial it returns that
// guard's (false, reason); on the first error it returns that error. Order matters only for
// which reason surfaces — approval/manual-lock reasons take precedence when listed first.
func (c Composite) CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error) {
	for _, g := range c.Guards {
		ok, reason, err := g.CanEdit(ctx, pageID, memberID, isAdmin)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, reason, nil
		}
	}
	return true, "", nil
}
