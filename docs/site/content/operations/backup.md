---
title: "Backup & Restore"
weight: 1
---

# Backup & Restore

## Export

```bash
curl -o backup.zip https://your-domain/api/backup/export \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN"
```

The ZIP contains:
- `manifest.json` — config (secrets redacted), webhook definitions, file inventory
- `data/*` — files from the Hub data directory

## Preview Before Restore

```bash
curl -X POST https://your-domain/api/backup/diff \
  -H "Content-Type: application/zip" \
  --data-binary @backup.zip
```

Returns what would change without applying anything.

## Restore

```bash
curl -X POST https://your-domain/api/backup/import \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/zip" \
  --data-binary @backup.zip
```

## Tor Key Backup

The Tor .onion address is derived from private keys in the `tor-keys` volume. Back them up separately:

```bash
docker run --rm -v meshsat-hub_tor-keys:/data -v $(pwd):/backup alpine \
  tar czf /backup/tor-keys-backup.tar.gz /data
```
