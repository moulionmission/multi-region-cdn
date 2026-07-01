#!/bin/bash
set -e

# Wait for primary DB to be ready
echo "Waiting for primary database (db-primary) to start..."
until pg_isready -h db-primary -p 5432 -U postgres; do
  sleep 1
done

echo "Primary database is online. Verifying replication connectivity..."

# Clear default data directory if standby has not been configured yet
if [ ! -s "$PGDATA/PG_VERSION" ]; then
    echo "Standby database directory is empty. Initiating pg_basebackup..."
    rm -rf "$PGDATA"/*
    
    # Run pg_basebackup. -R creates write standby.signal and setup replication connections in postgres.conf
    PGPASSWORD=repl_password pg_basebackup -h db-primary -D "$PGDATA" -U replicator -Fp -Xs -P -R
    
    echo "Backup completed successfully. Standby configured."
fi

# Set correct permissions
chmod 700 "$PGDATA"

# Handover execution back to default postgres entrypoint
echo "Starting PostgreSQL standby replica server..."
exec docker-entrypoint.sh postgres
