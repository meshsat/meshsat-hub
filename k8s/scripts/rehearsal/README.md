# Rehearsal and cutover data path (MESHSAT-944)

MariaDB Galera (NL) → Postgres (CNPG `meshsat-hub-main`) with pgloader, data only, into the
schema the Hub creates itself. Everything runs from the Claude runner; nothing new is
installed on the DMZ hosts.

```
dump-mariadb.sh                       # mariadb-dump from the NL container -> $SCRATCH/hub-<ts>.sql.gz (0600)
job-migrate.sh <rw-service>           # one-off k8s Job: Hub image --migrate-only against the target DB
meshsat.load.tmpl                     # pgloader command file (CASTs, data only, sequence reset AFTER LOAD)
cluster-drill.yaml                    # throwaway 1-instance CNPG cluster for the rehearsal
```
The two helpers that run containers on the operator host (a temporary MariaDB that imports
the dump, pgloader over a `kubectl port-forward`, and the per-table count / sha256
verification) live outside git in the CubeOS local scripts dir
(`~/gitlab/products/cubeos/scripts/meshsat-hub-rehearsal/{load-postgres.sh,verify-counts.sh}`),
because this repo's pre-commit rule forbids `docker run` in committed files. They take
`<dump.sql.gz> <rw-service>` and need `PGPASSWORD_MESHSAT` (OpenBao cnpg/meshsat_password).

## Rehearsal (phase 4)

1. `kubectl --context notrf01 apply -f cluster-drill.yaml` (1-instance throwaway CNPG cluster
   `meshsat-hub-drill` in `meshsat-hub-db`, same credentials Secret).
2. `./dump-mariadb.sh` (Galera read, `--single-transaction`; the hub keeps running).
3. `./job-migrate.sh meshsat-hub-drill-rw` — the Hub image with `--migrate-only` creates the
   schema (`schema_migrations` at the current version).
4. `./load-postgres.sh $SCRATCH/hub-<ts>.sql.gz meshsat-hub-drill-rw`.
5. `./verify-counts.sh $SCRATCH/hub-<ts>.sql.gz meshsat-hub-drill-rw` — every table's count must
   match; `system_config` `bridge_ca_cert`/`bridge_ca_key`, `reticulum_signing_key`/`reticulum_encryption_key`, `directory_signing_key` and `credential_master_key` (the
   secrets the bridges and devices depend on) are compared by sha256 of the raw value.
6. Bring a throwaway Hub up against the drill DB (`kubectl -n meshsat-hub set env deploy/hub
   HUB_DATABASE_URL=...` is NOT used: patch a copy of the Deployment named `hub-drill`), check
   `/readyz`, `GET /api/audit/verify` (chain intact), a pre-existing bridge certificate verifies
   (`POST /api/bridges/{id}/verify` or the fleet page), then delete `hub-drill` and the drill cluster.

## Cutover (phase 5, same tools)

`docker stop meshsat-hub` on NL and GR (write freeze) → `dump-mariadb.sh` → `job-migrate.sh
meshsat-hub-main-rw` → `load-postgres.sh ... meshsat-hub-main-rw` → `verify-counts.sh` → scale
`hub` to 1 → edge swap. The compose stacks are stopped, not removed, until the verification
in the plan passes; then phase 6 removes them.

## pgloader casts (meshsat.load.tmpl)

`tinyint(1)` → boolean, `datetime`/`timestamp` → timestamptz (zero dates → NULL),
`json`/`longtext` → text, `blob` → bytea, `bigint unsigned` → bigint. `schema_migrations` is
excluded (the Hub owns it). Sequences are reset by the `AFTER LOAD DO` block in the template (`reset-sequences.sql` is the same statement for a manual run). pgloader also tries to recreate the MariaDB `json_valid` CHECK constraints and reports them as errors; they do not exist in the Hub's Postgres schema and are ignored.
