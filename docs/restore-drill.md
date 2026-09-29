# Restoring the Hub's database from backup

This document is how to find out whether the Hub's database can be restored
from its backup, and it is written to be followed rather than read: every step
names what to expect and what it means if you get something else. It was first
run for real on 2026-09-15 (log at the end); the nightly verification job fails
once the last drill is older than 100 days, so it gets run again.

Do not treat a green backup as a proven backup: the drill below is what proves it.

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

**The usual cause of both looking wrong is not the database.** Since 2026-09-23
the bucket lives on Hetzner Object Storage behind the estate's in-cluster crypt
gateway (`backup-gateway.backup-gateway.svc:8080`, IFRNLLEI01PRD-2850); before
that it was NL SeaweedFS (nl-s3), which filled up four times and is now retired.
A gateway that is down, OOMKilled or refusing the namespace makes
`barman-cloud-wal-archive` fail for every consumer at once. Check for it before
blaming CNPG:

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
`~/gitlab/products/meshsat/scripts/meshsat-hub-rehearsal/` already computes
exactly this comparison; the same thing by hand is:

```bash
Q="select key, encode(sha256(value::bytea),'hex') from system_config
   where key in ('bridge_ca_cert',
                 'bridge_ca_key_enc','reticulum_signing_key_enc',
                 'reticulum_encryption_key_enc','directory_signing_key_enc',
                 'credential_master_key_enc')
   order by key"
diff <($LIVE "$Q") <($DRILL "$Q")
```

These must be **byte-identical**. Any difference is a failed drill, not a
rounding error.

⚠ **The `_enc` suffix is not a typo, and comparing the unsuffixed names instead
would pass for the wrong reason** (MESHSAT-1098, 2026-09-13). Those five values
are now AES-256-GCM sealed with `HUB_CONFIG_WRAP_KEY`, and the plaintext rows
they replaced are blanked — so the old query would compare two empty strings and
report a match. `bridge_ca_cert` stays unsuffixed because it is a public trust
anchor and is deliberately not sealed.

**A restore is therefore not enough on its own.** The sealed rows are
meaningless without the wrap key, which lives in OpenBao at
`ci-no/apps/meshsat-hub/hub`, property `HUB_CONFIG_WRAP_KEY`, and NOT in this
database. A drill that restores the database and forgets the wrap key produces a
Hub that refuses to start — which is the designed behaviour, because the
alternative is one that starts and regenerates every key. **Confirm the wrap key
is backed up somewhere the database restore does not depend on before calling
any drill green.**

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
kubectl --context notrf01 delete -f k8s/scripts/rehearsal/cluster-restore-drill.yaml   # the Cluster and its allow policy
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
- **Everything goes through one gateway.** Backups and restores both pass the
  in-cluster crypt gateway; if it is down, or its crypt key is lost, both are
  unavailable at once. The crypt key is escrowed in OpenBao and Vaultwarden, and
  losing it loses every backup.
- **Nothing before 2026-09-23 exists.** The history was copied from nl-s3 to the
  gateway the night of the move and nl-s3 was retired on 2026-09-25, so the
  oldest recoverability point is whatever the gateway holds.
- **The gateway was OOMKilled once by bulk copies** (8 parallel transfers,
  2026-09-25), which truncated a download; its limit is now 4Gi. Keep restores
  and bulk copies to a few transfers.

## Recording a drill

After a green drill, write the date into `k8s/verify/state-configmap.yaml`
(`RESTORE_DRILL_LAST`) and add a row below. The `hub-verify` CronJob reads that
ConfigMap every night and fails when the date is more than 100 days old
(MESHSAT-1152), which is the reminder to do this again.

One thing the row-count step will show that is NOT a failure:
`pg_stat_user_tables.n_live_tup` is planner statistics, and a restored cluster
starts with none, so most tables read 0 there until autovacuum has analysed
them. Compare `count(*)` on the must-match tables instead; that is what the log
below records.

## Drill log

| date | backup restored | time to healthy | schema | must-match tables | sealed keys | documents | result |
|---|---|---|---|---|---|---|---|
| 2026-09-15 | daily 2026-09-14 02:45 UTC + WAL, via `meshsat-hub-drill` (1 instance, control-plane tier) | 3 min 50 s | 25 = 25 | tenants 4, receipts 1, refunds 1, users 3, devices 2, bridges 3, system_config 14, credentials 1: all equal | 6 of 6 sha256 identical (`bridge_ca_cert` + the five `_enc` rows); `HUB_CONFIG_WRAP_KEY` confirmed present in OpenBao | receipts `issued` 1, max `MSH2026-0001`, refunds with credit note 1: equal | **PASS**; scratch cluster and PVC deleted, production pods untouched |
| 2026-09-29 | daily 2026-09-29 02:45 UTC + WAL to the latest segment (audit_log newest row and count equal to live), from Hetzner **through the backup gateway** for the first time (MESHSAT-1410); `meshsat-hub-drill`, 1 instance, with its own allow policy (the namespace is default-deny since MESHSAT-1205) | 2 min 16 s | 31 = 31 | tenants 6, receipts 1, refunds 1, users 4, devices 5, bridges 4, system_config 14, credentials 1: all equal | 6 of 6 sha256 identical; `HUB_CONFIG_WRAP_KEY` confirmed present in OpenBao | receipts `issued` 1, max `MSH2026-0001`, refunds with credit note 1: equal | **PASS**; drill cluster, policy and PVC deleted, production untouched |
