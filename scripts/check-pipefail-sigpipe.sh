#!/usr/bin/env bash
# Refuse `cmd | head` and friends inside scripts that set `pipefail`.
#
# WHY THIS EXISTS. Under `set -o pipefail` a reader that exits before its input
# is finished -- head, grep -q, grep -m, sed with an early q -- closes the pipe,
# the writer upstream takes SIGPIPE and returns 141, and pipefail promotes that
# into a failed script.
#
# It is a nasty failure because it depends on SIZE. While the producer's output
# fits in a pipe buffer (~64 KB) the writer finishes before the reader exits and
# everything passes: in review, in tests, on a small repo. It starts failing in
# production, on the day the input grows.
#
# On 2026-09-11 this took out EVERY deploy of meshsat-hub. `bump_k8s_pin` -- the
# job that deploys -- ended in `git log -100 ... | head -1` and returned 141 on a
# --depth 200 clone. Two merges sat built and undeployed until it was found. The
# same pattern silently rolled back a `git apply --3way` the same afternoon,
# reporting a clean apply having written nothing at all.
#
# Safe alternatives, in order of preference:
#   echo "$var" | grep -q X   ->  grep -q X <<<"$var"     (no pipe at all)
#   cmd | head -1             ->  cmd | sed -n 1p         (reads to EOF)
#   cmd | grep -q X           ->  out=$(cmd); grep -q X <<<"$out"
#
# `| tail -N` and a plain `| grep` are FINE: both read to EOF.
set -euo pipefail

root="${1:-.}"
status=0

# Files that arm pipefail are the only ones at risk.
mapfile -t armed < <(grep -rl --include='*.sh' --include='*.yml' --include='*.yaml' \
  -e 'pipefail' "$root" 2>/dev/null | grep -v node_modules | sort -u)

for f in "${armed[@]:-}"; do
  [ -n "$f" ] || continue
  # An early-exiting reader on the right-hand side of a pipe.
  hits=$(grep -nE '\|[[:space:]]*(head([[:space:]]|$)|grep[[:space:]]+-[a-zA-Z]*[qm]|sed[[:space:]]+-n[[:space:]]*.[0-9]+q)' "$f" 2>/dev/null || true)
  # Comment lines are not executed -- and this checker documents the very
  # pattern it looks for, so without this it fails on its own header.
  hits=$(printf '%s\n' "$hits" | grep -vE '^[0-9]+:[[:space:]]*#' || true)
  # An allow-list marker for the rare deliberate case.
  hits=$(printf '%s\n' "$hits" | grep -v 'sigpipe-ok' || true)
  if [ -n "$hits" ]; then
    status=1
    echo "FAIL $f"
    printf '%s\n' "$hits" | sed 's/^/     /'
  fi
done

if [ "$status" -ne 0 ]; then
  cat <<'MSG'

An early-exiting pipe reader in a script that sets `pipefail` will one day
return 141 and fail the job, once the producer's output outgrows a pipe buffer.
Use a herestring, or `sed -n 1p`, or capture to a variable first.

If a case is genuinely deliberate, put `sigpipe-ok` in a comment on that line.
MSG
else
  echo "no early-exiting pipe readers in pipefail scripts"
fi
exit "$status"
