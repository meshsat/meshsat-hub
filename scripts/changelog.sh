#!/usr/bin/env bash
# Generate changelog from conventional commits between two git tags.
# Usage: changelog.sh [from_tag] [to_tag]
# If no tags given, uses the last two tags.
# If only one tag exists, uses the initial commit as the starting point.
set -euo pipefail

# Determine tags
if [ $# -ge 2 ]; then
    FROM_TAG="$1"
    TO_TAG="$2"
elif [ $# -eq 1 ]; then
    TO_TAG="$1"
    FROM_TAG=$(git tag -l --sort=-v:refname | sed -n '2p' || true)
else
    TO_TAG=$(git tag -l --sort=-v:refname | sed -n '1p' || true)
    FROM_TAG=$(git tag -l --sort=-v:refname | sed -n '2p' || true)
fi

if [ -z "$TO_TAG" ]; then
    echo "No tags found. Nothing to generate." >&2
    exit 0
fi

# Determine range
if [ -z "$FROM_TAG" ]; then
    # No previous tag — use all commits up to TO_TAG
    RANGE="$TO_TAG"
    FROM_LABEL="initial commit"
else
    RANGE="${FROM_TAG}..${TO_TAG}"
    FROM_LABEL="$FROM_TAG"
fi

# Date of the TO_TAG commit
TAG_DATE=$(git log -1 --format='%cs' "$TO_TAG" 2>/dev/null || date +%Y-%m-%d)

# Collect commits
COMMITS=$(git log --format='%s (%h)' "$RANGE" 2>/dev/null || true)

if [ -z "$COMMITS" ]; then
    echo "No commits between ${FROM_LABEL} and ${TO_TAG}." >&2
    exit 0
fi

# Categorize commits
FEATS=""
FIXES=""
REFACTORS=""
DOCS=""
PERFS=""
TESTS=""
CHORES=""
OTHER=""

while IFS= read -r line; do
    # Extract type prefix (everything before the first colon or parenthesis)
    type=$(echo "$line" | sed -n 's/^\([a-z]*\)[(:].*/\1/p')
    case "$type" in
        feat)     FEATS="${FEATS}- ${line#feat:}
";;
        fix)      FIXES="${FIXES}- ${line#fix:}
";;
        refactor) REFACTORS="${REFACTORS}- ${line#refactor:}
";;
        docs)     DOCS="${DOCS}- ${line#docs:}
";;
        perf)     PERFS="${PERFS}- ${line#perf:}
";;
        test)     TESTS="${TESTS}- ${line#test:}
";;
        chore)    CHORES="${CHORES}- ${line#chore:}
";;
        *)        OTHER="${OTHER}- ${line}
";;
    esac
done <<< "$COMMITS"

# Output markdown
echo "# Changelog: ${FROM_LABEL} → ${TO_TAG} (${TAG_DATE})"
echo ""

if [ -n "$FEATS" ]; then
    echo "## Features"
    echo "$FEATS"
fi

if [ -n "$FIXES" ]; then
    echo "## Bug Fixes"
    echo "$FIXES"
fi

if [ -n "$REFACTORS" ]; then
    echo "## Refactoring"
    echo "$REFACTORS"
fi

if [ -n "$PERFS" ]; then
    echo "## Performance"
    echo "$PERFS"
fi

if [ -n "$DOCS" ]; then
    echo "## Documentation"
    echo "$DOCS"
fi

if [ -n "$TESTS" ]; then
    echo "## Tests"
    echo "$TESTS"
fi

if [ -n "$CHORES" ]; then
    echo "## Chores"
    echo "$CHORES"
fi

if [ -n "$OTHER" ]; then
    echo "## Other"
    echo "$OTHER"
fi
