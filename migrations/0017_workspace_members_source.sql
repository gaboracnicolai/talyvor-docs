-- Provenance for workspace_members: which system created each row.
--
-- WHY. Docs' tenancy has exactly one source today — the trackintegration syncer
-- pulling rosters from Track — which is why Docs cannot currently be sold without
-- Track. That coupling was inherited from a roster sync built for a different
-- purpose, not chosen, and the decision to give Docs its own tenancy root is
-- PARKED rather than closed (see internal/membership/store.go for the reopening
-- condition).
--
-- Keeping it open depends on a row the syncer did NOT create surviving a
-- reconcile. Before this column that held only by coincidence: a workspace Track
-- has never heard of pulls an empty roster and hits an empty-pull guard written
-- to survive a bad fetch, which says nothing about provenance — and inside a
-- MIXED workspace there was no protection at all, because a Docs-native member's
-- email is simply absent from Track's roster and the prune deletes what is
-- absent. This column makes the protection explicit and testable instead.
--
-- BACKFILL IS 'track' AND THAT IS CORRECT, not a convenience: at the moment this
-- runs the syncer is the only writer this table has ever had, so every existing
-- row genuinely came from Track.
ALTER TABLE workspace_members
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'track';

-- The prune filters on it (source = 'track'), and rosters are read per email.
CREATE INDEX IF NOT EXISTS idx_workspace_members_source ON workspace_members (source);
