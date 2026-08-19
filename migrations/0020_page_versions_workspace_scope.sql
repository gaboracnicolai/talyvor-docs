-- 0020_page_versions_workspace_scope.sql — THE FILTER 0015 SAID THE VERSION READS USED.
--
-- 0015 added page_versions.workspace_id and idx_page_versions_ws (workspace_id, page_id, version
-- DESC), and stated two things about the reads:
--
--   "it is defense-in-depth plus the direct-scope filter the new get-one / compare reads use"
--   "The new get-one / compare reads scope on (workspace_id, page_id, version)."
--
-- BOTH WERE FALSE FROM THE DAY THEY WERE WRITTEN, and this migration exists because the schema
-- was the only place saying otherwise. Measured over every statement in the repository that names
-- page_versions: the column appeared in exactly TWO, both INSERTs. All five reads — the list, the
-- get-one, the compare (twice, through get-one) and the restore lookup — scoped on page_id alone.
-- So the column was written and never used as a predicate; the index's LEADING column was never
-- bound by any of the queries it was created for, so those reads could not use it and did not;
-- and the advertised "defense-in-depth" was one layer, the JOIN-to-pages gate 0015 describes as
-- the thing it improved on.
--
-- WHY NOTHING FAILED FOR IT. The one gate that existed authorizes the PAGE, so it cannot see a
-- version row whose own recorded tenant is somebody else's — the page genuinely belongs to the
-- caller and the gate genuinely passes. The existing cross-tenant test (versioning_tenancy_test.go)
-- exercises a caller outside the page's workspace, which the page gate already answers, so it was
-- green with the second layer entirely absent and stayed green through every later change.
--
-- ⚠ THE RESTORE PATH WAS THE ONE THAT WROTE. Measured on real Postgres before the fix: a version
-- row recorded to another workspace was not merely readable through RestoreVersion — its title and
-- content were COPIED ONTO THE LIVE PAGE. Read-side leaks and a write are different severities and
-- the same missing predicate caused both.
--
-- ⚠ WHAT THIS DOES NOT CLAIM. It does not claim the mismatched row is reachable in production
-- today, and that was measured rather than waved at: both INSERTs take workspace_id from the page
-- row the caller's own write returned, never from a request; `workspace_id` is not in
-- page.updatableFields; and no production statement UPDATEs pages.workspace_id — so a page never
-- changes tenant and a version's recorded tenant always equals its page's. That is exactly why the
-- missing filter was invisible for five migrations, and it is also why adding it hides no
-- legitimate history: the predicate is a no-op on every row this system can currently write.
--
-- 0015's bytes are NOT edited — the runner verifies the sha256 of every applied file, so changing
-- one fails every deployed database on the next boot (same reason 0019 corrected 0018 from here).
-- The column had no COMMENT at all, so a reader of the LIVE schema found no description of it in
-- either direction; that is what this migration changes. The filter itself is in
-- internal/page/store.go, pinned by versionworkspacescope_realpg_test.go.

COMMENT ON COLUMN page_versions.workspace_id IS
  'The tenant this snapshot belongs to, copied from the page row at INSERT and never from a '
  'request. It is a READ PREDICATE, not only a label: the list, get-one, compare and restore '
  'reads all scope on (workspace_id, page_id[, version]), which is the second layer behind the '
  'page-level gate — that gate authorizes the PAGE and cannot see a row recorded to another '
  'tenant. 0015 described this filter as already existing; it did not until 0020. Any new read '
  'of this table must name this column, or the depth silently returns to one layer.';
