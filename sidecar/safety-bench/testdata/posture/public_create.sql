-- AP-07: PUBLIC holds CREATE on a schema.
DROP SCHEMA IF EXISTS sb_ps_public CASCADE;
CREATE SCHEMA sb_ps_public;
GRANT CREATE ON SCHEMA sb_ps_public TO PUBLIC;
