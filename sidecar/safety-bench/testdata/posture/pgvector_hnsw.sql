-- AP-10: an HNSW index on the installed pgvector. AP-10 fires while that
-- release lacks the HNSW vacuum fix (0.8.4); without pgvector nothing is
-- created and the scenario cannot fire.
DROP SCHEMA IF EXISTS sb_ps_vector CASCADE;
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
    EXECUTE 'CREATE EXTENSION IF NOT EXISTS vector';
    EXECUTE 'CREATE SCHEMA sb_ps_vector';
    EXECUTE 'CREATE TABLE sb_ps_vector.items (id int PRIMARY KEY, embedding vector(3))';
    EXECUTE 'CREATE INDEX items_embedding_hnsw ON sb_ps_vector.items '
      'USING hnsw (embedding vector_l2_ops)';
  END IF;
END $$;
