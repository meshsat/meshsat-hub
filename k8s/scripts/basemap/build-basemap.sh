#!/usr/bin/env bash
# Build and publish the Hub's self-hosted vector basemap (MESHSAT-967).
#
# Extracts a world basemap from the Protomaps daily planet build over HTTP range
# requests (no full planet download), uploads it and the glyph and sprite assets
# to the Hub's object store, and prints the two config values to set. The Hub
# streams the result at /basemap/, so no browser ever contacts a tile host.
#
#   build-basemap.sh [YYYYMMDD] [maxzoom]
#
# Defaults: yesterday's planet build, maxzoom 8 (~530 MB world; z7 is ~180 MB
# and z6 ~45 MB. Above z8 the archive grows fast; check the object store has the
# room before going higher).
#
# Needs: curl, aws CLI, and the go-pmtiles binary on PATH (or PMTILES=/path).
# Credentials: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY of the bucket, e.g.
#   export AWS_ACCESS_KEY_ID=$(kubectl -n meshsat-hub get secret hub-secrets \
#     -o jsonpath='{.data.HUB_AUDIT_ARCHIVE_S3_ACCESS_KEY}' | base64 -d)
set -euo pipefail

BUILD="${1:-$(date -u -d yesterday +%Y%m%d)}"
MAXZOOM="${2:-8}"
ENDPOINT="${S3_ENDPOINT:-https://nl-s3.nuclearlighters.net}"
BUCKET="${S3_BUCKET:-cnpg-meshsat-hub}"
PREFIX="${S3_PREFIX:-basemap}"
PMTILES="${PMTILES:-pmtiles}"
WORK="${WORK:-$(mktemp -d)}"
PLANET="https://build.protomaps.com/${BUILD}.pmtiles"
ARCHIVE="protomaps-world-z${MAXZOOM}-${BUILD}.pmtiles"

command -v "$PMTILES" >/dev/null || { echo "go-pmtiles not found; see https://github.com/protomaps/go-pmtiles/releases" >&2; exit 1; }
: "${AWS_ACCESS_KEY_ID:?set AWS_ACCESS_KEY_ID}" "${AWS_SECRET_ACCESS_KEY:?set AWS_SECRET_ACCESS_KEY}"
curl -sfI "$PLANET" >/dev/null || { echo "no planet build at $PLANET (builds are kept about a week)" >&2; exit 1; }

echo "extracting world z0-${MAXZOOM} from ${BUILD}"
"$PMTILES" extract "$PLANET" "$WORK/$ARCHIVE" --maxzoom="$MAXZOOM" --bbox=-180,-85.05,180,85.05

echo "fetching the glyph and sprite assets"
git clone --depth 1 -q https://github.com/protomaps/basemaps-assets.git "$WORK/assets"

echo "uploading to s3://$BUCKET/$PREFIX/"
aws --endpoint-url "$ENDPOINT" s3 cp "$WORK/$ARCHIVE" "s3://$BUCKET/$PREFIX/$ARCHIVE" --no-progress
aws --endpoint-url "$ENDPOINT" s3 sync "$WORK/assets/fonts" "s3://$BUCKET/$PREFIX/assets/fonts" --no-progress --exclude '*Devanagari*'
aws --endpoint-url "$ENDPOINT" s3 sync "$WORK/assets/sprites/v4" "s3://$BUCKET/$PREFIX/assets/sprites/v4" --no-progress

cat <<MSG

done. Set in k8s/hub/configmap.yaml and let Argo roll the Hub:

  HUB_BASEMAP_S3_KEY: "$PREFIX/$ARCHIVE"
  HUB_BASEMAP_S3_ASSET_PREFIX: "$PREFIX/assets"

Then delete the previous archive object once the new pin is live.
Fonts are Noto Sans under the SIL Open Font License (OFL.txt in the assets repo);
tiles are OpenStreetMap data under ODbL, credited in the map's attribution control.
MSG
