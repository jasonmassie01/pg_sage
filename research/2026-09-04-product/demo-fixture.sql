-- Synthetic fixture for an explicitly designated disposable PostgreSQL database.
-- The lab itself never runs this file and never creates objects.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE SCHEMA pgsage_vectorlab_demo;
CREATE TABLE pgsage_vectorlab_demo.documents (
    id integer PRIMARY KEY,
    tenant integer NOT NULL,
    embedding vector(2) NOT NULL
);
INSERT INTO pgsage_vectorlab_demo.documents
SELECT i, i % 5, ('[' || i || ',0]')::vector
FROM generate_series(1, 10000) i;
CREATE INDEX documents_embedding_hnsw
ON pgsage_vectorlab_demo.documents USING hnsw (embedding vector_l2_ops);
ANALYZE pgsage_vectorlab_demo.documents;
