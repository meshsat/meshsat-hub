#!/usr/bin/env bash
# migrate-sqlite-to-pg.sh — Export standalone SQLite data and import into PostgreSQL.
#
# Usage:
#   ./scripts/migrate-sqlite-to-pg.sh /path/to/hub.db "postgres://user:pass@host:5432/meshsat_hub"
#
# Prerequisites:
#   - sqlite3 CLI
#   - psql CLI
#   - PostgreSQL target must have tables created (run Hub once in cluster mode first)
#
# The script exports each table to CSV, then uses COPY to load into PostgreSQL.
# It is idempotent — re-running will skip rows that already exist (ON CONFLICT DO NOTHING).

set -euo pipefail

SQLITE_DB="${1:?Usage: $0 <sqlite-db-path> <postgres-url>}"
PG_URL="${2:?Usage: $0 <sqlite-db-path> <postgres-url>}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "=== MeshSat Hub: SQLite → PostgreSQL Migration ==="
echo "Source: $SQLITE_DB"
echo "Target: $PG_URL"
echo "Temp dir: $TMPDIR"
echo ""

TABLES=(
    "devices"
    "messages"
    "positions"
    "audit_log"
    "webhook_configs"
    "delivery_logs"
    "api_keys"
    "device_configs"
)

# Export each table from SQLite to CSV
for table in "${TABLES[@]}"; do
    echo "Exporting $table..."
    sqlite3 -header -csv "$SQLITE_DB" "SELECT * FROM $table;" > "$TMPDIR/$table.csv" 2>/dev/null || {
        echo "  (table $table not found in SQLite — skipping)"
        continue
    }
    rows=$(wc -l < "$TMPDIR/$table.csv")
    rows=$((rows - 1))  # subtract header
    echo "  $rows rows exported"
done

echo ""
echo "Importing into PostgreSQL..."

for table in "${TABLES[@]}"; do
    csv="$TMPDIR/$table.csv"
    [ -f "$csv" ] || continue
    rows=$(wc -l < "$csv")
    rows=$((rows - 1))
    [ "$rows" -le 0 ] && { echo "  $table: 0 rows — skipping"; continue; }

    # Get column names from CSV header
    columns=$(head -1 "$csv")

    echo "  Importing $table ($rows rows)..."

    # Create a temp table, COPY into it, then INSERT ... ON CONFLICT DO NOTHING
    psql "$PG_URL" -q <<SQL
CREATE TEMP TABLE _import_${table} (LIKE ${table} INCLUDING ALL);
\\COPY _import_${table}(${columns}) FROM '${csv}' WITH (FORMAT csv, HEADER true);
INSERT INTO ${table}(${columns})
SELECT ${columns} FROM _import_${table}
ON CONFLICT DO NOTHING;
DROP TABLE _import_${table};
SQL

    echo "  $table: done"
done

echo ""
echo "=== Migration complete ==="
echo "Verify with: psql '$PG_URL' -c 'SELECT tablename, n_live_tup FROM pg_stat_user_tables ORDER BY tablename;'"
