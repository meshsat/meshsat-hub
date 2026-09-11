# Restoring the Hub's database from backup

The Hub's database has never been restored from a backup. This document is how
to find out whether it can be, and it is written to be followed rather than
read: every step names what to expect and what it means if you get something
else.

Do not treat a green backup as a proven backup. `k8s/NOTES.md` has asked for
this drill since the cutover and it has never been run.

## Why this one matters more than most

`meshsat-hub-main` stores on `local-hostpath-retain` — node-local, with no
cross-node re-attach. Its pgdata is *deliberately* excluded from Velero
(`backup.velero.io/backup-volumes-excludes: pgdata` on the Cluster), because
barman is meant to be the path. So the barman copy in `s3://cnpg-meshsat-hub`
is **the only copy of this database that is not on a single node's disk**.

It also holds legal documents. Receipts and credit notes carry numbers out of a
gapless series; losing the table that records which numbers were issued is worse
than losing the rows, because the next document would reuse a number.

## Before you start: is there anything to restore?

Two checks, in this order. A restore proves nothing about a backup that was
never taken.

```bash
K="kubectl --context notrf01 -n meshsat-hub-db"

# 1. Is WAL archiving working right now?
$K get cluster meshsat-hub-main \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
```

`ContinuousArchiving=True` is what you want. `False` means WAL is piling up on
the primary's volume and the recoverable window has stopped advancing, whatever
the last base backup says.

```bash
# 2. How old is the newest usable backup, and how far back can you go?
$K get cluster meshsat-hub-main -o jsonpath='{"last:  "}{.status.lastSuccessfulBackup}{"\nfirst: "}{.status.firstRecoverabilityPoint}{"\n"}'
$K get backups
```

**The usual cause of both looking wrong is not the database.** The bucket lives
on NL SeaweedFS, which is a different site reached over the wire — notrf01 has
no local backup target. When that fills up, `barman-cloud-wal-archive` fails
with `PutObject ... (InternalError) (reached max retries: 4)` and every
collection in the estate stops accepting writes at once. That is what happened
on 2026-09-11 (IFRNLLEI01PRD-2831). Check for it before blaming CNPG:

```bash
kubectl --context notrf01 -n meshsat-hub-db logs meshsat-hub-main-1 -c postgres --tail=200 \
  | grep -i barman
```

## The drill

```bash
kubectl --context notrf01 apply -f k8s/scripts/rehearsal/cluster-restore-drill.yaml
kubectl --context notrf01 -n meshsat-hub-db get cluster meshsat-hub-drill -w
```

The manifest beside this document carries the reasoning for each field. Three
that are easy to get wrong and produce confusing failures:

- `externalClusters[].barmanObjectStore.serverName` must be **`meshsat-hub-main`**,
  the name the live cluster writes under — not the drill cluster's own name. Get
  this wrong and you get "no backup found" against a bucket that has one.
- `imageName` must be the same PostgreSQL major as the source (18) or newer. An
  older image fails *after* pulling the base backup down.
- There is deliberately **no `backup:` stanza**. A drill cluster must not be able
  to write to the bucket it is reading.

Expect the pod to go `Initializing` → `Ready` in a couple of minutes; the
database is about 12 MB.

If it never leaves `Initializing`, read the job logs — the restore runs as a
one-off job, not in the instance pod:

```bash
kubectl --context notrf01 -n meshsat-hub-db logs -l cnpg.io/jobRole=full-recovery --tail=100
```

## Verifying the restore, rather than admiring it

A cluster that reaches `Ready` has proved that the base backup downloads and
PostgreSQL starts. It has not proved the data is there.

```bash
DRILL="kubectl --context notrf01 -n meshsat-hub-db exec meshsat-hub-drill-1 -- psql -U postgres -d meshsat_hub -t -A -c"
LIVE="kubectl  --context notrf01 -n meshsat-hub-db exec meshsat-hub-main-1  -- psql -U postgres -d meshsat_hub -t -A -c"
```

**1. Row counts, per table.** Not a total — a total hides one table being empty
while another double-counted.

```bash
Q="select relname, n_live_tup from pg_stat_user_tables order by relname"
diff <($LIVE "$Q") <($DRILL "$Q")
```

Small differences on high-churn tables (`messages`, `positions`, `audit_log`)
are expected: the live cluster kept taking traffic after the backup was cut. The
tables that must match exactly are the ones nobody writes to casually:
`tenants`, `receipts`, `refunds`, `users`, `devices`, `bridges`,
`schema_migrations`, `system_config`.

**2. The crown jewels.** `system_config` holds key material that cannot be
regenerated without re-onboarding every bridge: lose these and every field
device's certificate has to be reissued. `verify-counts.sh` in
`~/gitlab/products/cubeos/scripts/meshsat-hub-rehearsal/` already computes
exactly this comparison; the same thing by hand is:

```bash
Q="select key, encode(sha256(value::bytea),'hex') from system_config
   where key in ('bridge_ca_cert','bridge_ca_key','reticulum_signing_key',
                 'reticulum_encryption_key','directory_signing_key','credential_master_key')
   order by key"
diff <($LIVE "$Q") <($DRILL "$Q")
```

These must be **byte-identical**. Any difference is a failed drill, not a
rounding error.

**3. The document series.** The one thing a wrong restore would quietly corrupt:

```bash
$DRILL "select status, count(*) from receipts group by status"
$DRILL "select max(invoice_number) from receipts where invoice_number <> ''"
$DRILL "select count(*) from refunds where credit_ref <> ''"
```

Compare with live. A receipt that exists in the backup but not in live (or the
reverse) is worth understanding before you dismiss it.

**4. Schema version.** A restore that silently lands on an older migration is
the kind of thing that only shows up later:

```bash
$DRILL "select max(version) from schema_migrations"   # expect the same as live
```

## Tear down

```bash
kubectl --context notrf01 -n meshsat-hub-db delete cluster meshsat-hub-drill
kubectl --context notrf01 -n meshsat-hub-db get pvc | grep drill    # expect nothing
```

The drill uses `local-hostpath-delete`, so its PV goes with it. Check anyway —
an orphaned PV on a control-plane node is exactly the sort of thing that is
still there a year later.

## What a real restore would look like

This drill restores *beside* the live cluster. Restoring *over* it is a
different operation and is not covered here, deliberately: it is a decision
somebody makes under pressure, and the safe shape is the same one — bring up a
new cluster from the bucket, verify it with the checks above, and only then
repoint the Hub at it by changing the connection Secret. Never recover in place
while the old primary is still running.

## Known limits, stated so they are not discovered mid-incident

- **Retention is 14 days and has never been exercised.** The cluster was
  bootstrapped on 2026-09-08, so nothing has aged out yet. The first expiry will
  be the first time that code path runs here.
- **The bucket is at another site.** notrf01 has no local backup target; if NL
  SeaweedFS is down or full, both the backups and the restores are unavailable
  at once. There is a documented estate incident of exactly this.
- **The S3 gateway has truncated a restore before.** The NL `seaweedfs-s3` vhost
  once cut a 2 GB base tar at exactly 512 MiB with a clean EOF, and production
  backups were not restorable through it until ModSecurity and proxy buffering
  were turned off on that vhost. That fix is in the NL infrastructure repo. At
  12 MB this database is nowhere near the threshold, but it will not always be.
