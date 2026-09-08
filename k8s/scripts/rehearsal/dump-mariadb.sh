#!/usr/bin/env bash
# mariadb-dump of the live Hub database from the NL DMZ MariaDB container (MESHSAT-944).
# Read-only on the host: the root password travels as a defaults file on stdin into
# the container's /tmp and is removed afterwards; nothing lands in argv or the host.
# Output: $OUT (default: the session scratchpad), mode 0600, gzip.
set -euo pipefail
HOST="${DUMP_HOST:-nllei01dmz01}"
CONTAINER="${DUMP_CONTAINER:-meshsat-mariadb}"
DB="${DUMP_DB:-meshsat_hub}"
OUT="${OUT:-${SCRATCH:-/tmp}/hub-$(date -u +%Y%m%dT%H%M%SZ).sql.gz}"
: "${MARIADB_ROOT_PASSWORD:?export MARIADB_ROOT_PASSWORD (from the captured .env), never on the command line}"
umask 077
printf '[client]\nuser=root\npassword=%s\n' "$MARIADB_ROOT_PASSWORD" \
  | ssh -i ~/.ssh/one_key -o User=kyriakosp -o BatchMode=yes "$HOST" \
      "docker exec -i $CONTAINER sh -c 'cat > /tmp/.dump.cnf; mariadb-dump --defaults-extra-file=/tmp/.dump.cnf --single-transaction --quick --skip-lock-tables --routines=0 --triggers=0 --events=0 --no-tablespaces --hex-blob $DB; rc=\$?; rm -f /tmp/.dump.cnf; exit \$rc' | gzip -c" > "$OUT"
size=$(stat -c %s "$OUT")
[ "$size" -gt 1024 ] || { echo "FATAL: dump too small ($size bytes)"; exit 1; }
echo "dump: $OUT ($size bytes, $(zcat "$OUT" | grep -c '^INSERT INTO') INSERT statements)"
