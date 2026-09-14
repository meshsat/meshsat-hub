#!/usr/bin/env bash
# Refuse a HUB_* key that a Kubernetes manifest supplies and no Go code reads.
#
# WHY THIS EXISTS. On 2026-09-14 an audit of all 173 HUB_* variables found that
# k8s/hub/externalsecret.yaml had always supplied HUB_CLOUDLOOP_MQTT_CA while
# internal/config/config.go only ever read HUB_CLOUDLOOP_MQTT_CA_CERT. The names
# never matched, so CACertFile reached the Cloudloop MQTT subscriber EMPTY and
# the pin to Cloudloop's own CA silently never applied -- the connection fell
# back to Go's system root pool instead.
#
# That was harmless in the end (the file is the Amazon root CA, which is in the
# system pool anyway), but nothing would have said so either way. The manifest
# claimed one thing, the binary did another, and the two were never compared.
# A misnamed key is indistinguishable from an unset one: every `!= ""` guard in
# config.go treats it as "not configured" and takes a default, quietly.
#
# So this gate is one-directional and that is deliberate:
#
#   supplied by a manifest, never read by Go  ->  FAIL. Someone believes this
#       key does something. It does nothing. Either the name is wrong or the
#       key is dead; both are worth a human deciding.
#
#   read by Go, supplied by no manifest       ->  FINE. That is every optional
#       setting with a default, which is most of them.
#
# If a key is genuinely meant to be consumed by something other than the Hub
# binary (a sidecar, an init container, a reloader), put `env-key-ok` in a
# comment on the same line.
set -euo pipefail

root="${1:-.}"
status=0

# Every HUB_* string literal in the Go source -- NOT just os.Getenv("HUB_X").
# Matching only os.Getenv was the first version of this script and it produced 23
# false positives on its first run, because config.go reaches env in three shapes:
#
#   os.Getenv("HUB_LOG_LEVEL")                     a plain call
#   stripeSecret("HUB_STRIPE_SECRET_KEY")          a helper that wraps os.Getenv
#   "HUB_BASEMAP_S3_KEY": &cfg.BasemapS3Key        a map of name -> destination
#   os.Getenv("HUB_PLAN_" + upper(plan) + "_DEVICES")   a name built in a loop
#
# The first three are caught by scanning for the literal. The fourth cannot be,
# because the full key never appears anywhere -- so a literal ending in `_` is
# treated as a PREFIX and anything starting with it counts as read. That is
# deliberately generous: this gate exists to catch a name that matches nothing,
# and a prefix that matches is evidence enough that somebody is reading the family.
read_keys=$(grep -rhoE '"HUB_[A-Z0-9_]*"' "$root" --include='*.go' 2>/dev/null \
  | tr -d '"' | sort -u || true)

if [ -z "$read_keys" ]; then
  echo "check-env-keys: found no HUB_* literals in any Go source -- refusing to pass vacuously" >&2
  exit 1
fi

# Prefix fragments: literals that end in `_` and so are concatenated, never used whole.
prefixes=$(grep -E '_$' <<<"$read_keys" || true)

is_read() {
  local key="$1" p
  grep -qx "$key" <<<"$read_keys" && return 0
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    case "$key" in "$p"*) return 0 ;; esac
  done <<<"$prefixes"
  return 1
}

# Every HUB_* key a manifest supplies, as a ConfigMap/Secret data key or as an
# ExternalSecret template key. Both are `KEY:` at the start of a YAML mapping.
# `name: HUB_X` (a Deployment env entry) is a REFERENCE, not a supply, and is
# covered by the same check on whatever backs it.
supplied=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  hits=$(grep -nE '^[[:space:]]*HUB_[A-Z0-9_]+[[:space:]]*:' "$f" 2>/dev/null || true)
  hits=$(printf '%s\n' "$hits" | grep -v 'env-key-ok' || true)
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    key=$(printf '%s' "$line" | grep -oE 'HUB_[A-Z0-9_]+' | sed -n 1p)
    [ -n "$key" ] || continue
    if ! is_read "$key"; then
      status=1
      supplied="${supplied}${f}:${line%%:*}  ${key}"$'\n'
    fi
  done <<<"$hits"
done < <(find "$root/k8s" -name '*.yaml' -type f 2>/dev/null | sort)

if [ "$status" -ne 0 ]; then
  echo "FAIL: these keys are supplied by a manifest and read by no Go code"
  printf '%s' "$supplied" | sed 's/^/     /'
  cat <<'MSG'

A key nothing reads is either misnamed or dead. A misnamed one is the worse
case: config.go cannot tell it apart from unset, so the setting silently takes
its default and the manifest goes on claiming otherwise.

Fix the name to match internal/config/config.go, or delete the key. If it is
consumed by something other than the Hub binary, put `env-key-ok` in a comment
on that line.
MSG
else
  echo "every HUB_* key supplied by a manifest is read by the Hub"
fi
exit "$status"
