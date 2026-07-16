-- 0015_page_versions_workspace.sql — tenancy column on the version history.
--
-- page_versions (created in 0002_pages.sql) carried no workspace_id: tenancy was enforced
-- only by JOINing to pages.workspace_id via the store's *InWorkspaces gates. The
-- single-writer + versioning work makes the version row self-describing — it carries its own
-- workspace_id, so a read can scope directly on the table and an auditor knows a row's tenant
-- without a JOIN. This does NOT relax enforcement (the store still gates every version op
-- through the page's workspace); it is defense-in-depth plus the direct-scope filter the new
-- get-one / compare reads use.

ALTER TABLE page_versions ADD COLUMN IF NOT EXISTS workspace_id TEXT;

-- Backfill existing rows from their page's workspace. Every version references a page
-- (FK page_id, ON DELETE CASCADE), so every row gets a tenant.
UPDATE page_versions pv
SET    workspace_id = p.workspace_id
FROM   pages p
WHERE  pv.page_id = p.id
  AND  pv.workspace_id IS NULL;

-- Every version now has a tenant; enforce it. After this migration the store always supplies
-- workspace_id on insert, so no version can exist without one.
ALTER TABLE page_versions ALTER COLUMN workspace_id SET NOT NULL;

-- The new get-one / compare reads scope on (workspace_id, page_id, version).
CREATE INDEX IF NOT EXISTS idx_page_versions_ws
    ON page_versions (workspace_id, page_id, version DESC);
