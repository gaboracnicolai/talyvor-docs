-- 0004_search.sql — Phase 6 search enhancements.
--
-- Adds pgvector-backed semantic embeddings and a weighted full-text
-- index that ranks title hits above body hits. The vector extension
-- is REQUIRED for this migration; deployments without pgvector will
-- fall back to full-text-only search (semantic.IsEnabled() returns
-- false at runtime).
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS page_embeddings (
    page_id    TEXT PRIMARY KEY REFERENCES pages(id) ON DELETE CASCADE,
    embedding  vector(1536),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- ivfflat is good enough for tens-of-thousands of pages; revisit for
-- hnsw once Docs deployments grow into the hundreds of thousands.
-- 100 lists is the rule-of-thumb default for ≤1M rows.
CREATE INDEX IF NOT EXISTS idx_page_embeddings_vector
    ON page_embeddings
    USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

-- Weighted full-text index. Title hits (A) outrank body hits (B);
-- the existing unweighted index is dropped so ranking math stays
-- consistent with the SearchWithRank query.
DROP INDEX IF EXISTS idx_pages_search;
CREATE INDEX IF NOT EXISTS idx_pages_search_weighted ON pages
    USING gin((
        setweight(to_tsvector('english', title), 'A') ||
        setweight(to_tsvector('english', content_text), 'B')
    ));
