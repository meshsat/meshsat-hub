#!/usr/bin/env bash
# Build and publish the Hub's self-hosted vector basemap (MESHSAT-967).
#
# Extracts a world basemap from the Protomaps daily planet build over HTTP range
# requests (no full planet download), uploads it and the glyph and sprite assets
# to the Hub's object store, and prints the two config values to set. The Hub
# streams the result at /basemap/, so no browser ever contacts a tile host.
#
#   build-basemap.sh [YYYYMMDD] [maxzoom] [bbox] [name]
#
# Two archives make the map: a shallow world one for context and a deeper one
# for the area the fleet operates in. That split is not an optimisation, it is
# forced: in this schema street geometry appears at zoom 13 and street NAMES at
# zoom 15, and a world archive that deep does not fit anywhere.
#
#   measured against the 2026-09-07 planet build
#   world z0-8    0.5 GB   coastlines, borders, city dots. No streets.
#   world z0-10   3.6 GB
#   world z0-11   7.9 GB   major roads and their names
#   Europe z0-13  9.3 GB   residential street geometry, no street names
#   NL z0-15      2.0 GB   everything, including street names
#
# So: world at 11, and one deeper archive per operating area at 15.
#
#   build-basemap.sh 20260907 11                        # the world
#   build-basemap.sh 20260907 15 @fleet-regions.geojson fleet   # where the fleet is
#
# fleet-regions.geojson beside this script holds one polygon per country the
# fleet operates in. Add a country there and rebuild; the Netherlands and
# Greece together come to 2.6 GB at zoom 15.
#
# Point HUB_BASEMAP_S3_KEY at the world archive and HUB_BASEMAP_S3_LOCAL_KEY at
# the deep one. The map draws the deep layers on top from zoom 11; outside their
# coverage there are simply no tiles and the world layers stay visible.
#
# Needs: curl, aws CLI, and the go-pmtiles binary on PATH (or PMTILES=/path).
# Credentials: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY of the bucket, e.g.
#   export AWS_ACCESS_KEY_ID=$(kubectl -n meshsat-hub get secret hub-secrets \
#     -o jsonpath='{.data.HUB_AUDIT_ARCHIVE_S3_ACCESS_KEY}' | base64 -d)
set -euo pipefail

BUILD="${1:-$(date -u -d yesterday +%Y%m%d)}"
MAXZOOM="${2:-11}"
# A bbox, or @file for a GeoJSON polygon set (one archive, several countries).
BBOX="${3:--180,-85.05,180,85.05}"
NAME="${4:-world}"
ENDPOINT="${S3_ENDPOINT:-https://nl-s3.nuclearlighters.net}"
BUCKET="${S3_BUCKET:-cnpg-meshsat-hub}"
PREFIX="${S3_PREFIX:-basemap}"
PMTILES="${PMTILES:-pmtiles}"
WORK="${WORK:-$(mktemp -d)}"
PLANET="https://build.protomaps.com/${BUILD}.pmtiles"
ARCHIVE="protomaps-${NAME}-z${MAXZOOM}-${BUILD}.pmtiles"

command -v "$PMTILES" >/dev/null || { echo "go-pmtiles not found; see https://github.com/protomaps/go-pmtiles/releases" >&2; exit 1; }
: "${AWS_ACCESS_KEY_ID:?set AWS_ACCESS_KEY_ID}" "${AWS_SECRET_ACCESS_KEY:?set AWS_SECRET_ACCESS_KEY}"
curl -sfI "$PLANET" >/dev/null || { echo "no planet build at $PLANET (builds are kept about a week)" >&2; exit 1; }

echo "extracting ${NAME} z0-${MAXZOOM} from ${BUILD}"
case "$BBOX" in
  @*) REGION="$(dirname "$0")/${BBOX#@}"
      [ -f "$REGION" ] || { echo "no region file at $REGION" >&2; exit 1; }
      "$PMTILES" extract "$PLANET" "$WORK/$ARCHIVE" --maxzoom="$MAXZOOM" --region="$REGION" ;;
   *) "$PMTILES" extract "$PLANET" "$WORK/$ARCHIVE" --maxzoom="$MAXZOOM" --bbox="$BBOX" ;;
esac

echo "fetching the glyph and sprite assets"
git clone --depth 1 -q https://github.com/protomaps/basemaps-assets.git "$WORK/assets"

echo "uploading to s3://$BUCKET/$PREFIX/"
aws --endpoint-url "$ENDPOINT" s3 cp "$WORK/$ARCHIVE" "s3://$BUCKET/$PREFIX/$ARCHIVE" --no-progress
aws --endpoint-url "$ENDPOINT" s3 sync "$WORK/assets/fonts" "s3://$BUCKET/$PREFIX/assets/fonts" --no-progress --exclude '*Devanagari*'
aws --endpoint-url "$ENDPOINT" s3 sync "$WORK/assets/sprites/v4" "s3://$BUCKET/$PREFIX/assets/sprites/v4" --no-progress

cat <<MSG

done. Set in k8s/hub/configmap.yaml and let Argo roll the Hub:

  HUB_BASEMAP_S3_KEY: "$PREFIX/$ARCHIVE"        # if this was the world archive
  HUB_BASEMAP_S3_LOCAL_KEY: "$PREFIX/$ARCHIVE"  # if this was a regional one
  HUB_BASEMAP_S3_ASSET_PREFIX: "$PREFIX/assets"

Then delete the previous archive object once the new pin is live.
Fonts are Noto Sans under the SIL Open Font License (OFL.txt in the assets repo);
tiles are OpenStreetMap data under ODbL, credited in the map's attribution control.
MSG
