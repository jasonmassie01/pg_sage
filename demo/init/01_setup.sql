-- Enable pg_stat_statements (must be done as superuser)
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- Create sage_agent user
CREATE USER sage_agent WITH PASSWORD 'sage_password';

-- Grant required privileges
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON DATABASE sage_demo TO sage_agent;

-- Create sage schema owned by sage_agent
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
GRANT ALL ON SCHEMA sage TO sage_agent;
GRANT ALL ON SCHEMA public TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO sage_agent;

-- Grant sequence usage for serial columns
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE ON SEQUENCES TO sage_agent;
ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT USAGE ON SEQUENCES TO sage_agent;
