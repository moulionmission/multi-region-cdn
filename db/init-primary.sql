-- Primary DB Schema and Replication User Setup
CREATE TABLE IF NOT EXISTS urls (
    code VARCHAR(50) PRIMARY KEY,
    long_url TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Create replication user for streaming replicas
-- We use a DO block to prevent error if role already exists
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'replicator') THEN
        CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD 'repl_password';
    END IF;
END $$;
