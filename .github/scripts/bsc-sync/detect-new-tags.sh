#!/usr/bin/env bash
#
# detect-new-tags.sh — list upstream BSC tags newer than the last-synced tag.
#
# Prints the new tags in ascending (merge) order, one per line, on stdout.
# All diagnostics go to stderr so stdout can be captured verbatim by the
# workflow. Exits 0 with empty stdout when there is nothing to sync.
#
# Major releases (a tag whose major version increases vs. the last-synced
# tag, e.g. v1.7.3 -> v2.0.0) are NOT auto-synced — they are too significant
# to merge by AI. By default this script stops the syncable list before the
# first such tag and reports the held-back major(s) separately, so the caller
# can alert a human instead of merging (see COR-37, Step 2b).
#
# Env:
#   BSC_REMOTE            remote name for bnb-chain/bsc (default: bsc)
#   BSC_SYNC_STATE_FILE   path to the last-synced-tag file
#                         (default: <repo>/.github/bsc-sync/last-synced-tag)
#   INCLUDE_PRERELEASE    "1" to include -alpha/-beta/-rc/-feature tags
#                         (default: 0 — stable vX.Y.Z tags only)
#   STOP_AT_MAJOR         "1" to hold back tags from the first major-version
#                         bump onward (default: 1). Set "0" to treat majors
#                         like any other tag (e.g. an explicit manual run).
#   MAJOR_TAGS_FILE       optional path; if set, the held-back major-boundary
#                         tags are written here (one per line, possibly empty).
#
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
BSC_REMOTE="${BSC_REMOTE:-bsc}"
STATE_FILE="${BSC_SYNC_STATE_FILE:-$REPO_ROOT/.github/bsc-sync/last-synced-tag}"
INCLUDE_PRERELEASE="${INCLUDE_PRERELEASE:-0}"
STOP_AT_MAJOR="${STOP_AT_MAJOR:-1}"
MAJOR_TAGS_FILE="${MAJOR_TAGS_FILE:-}"

log() { printf '%s\n' "$*" >&2; }

# Major version component of a vX.Y.Z tag (e.g. v2.0.0 -> 2). Empty if the
# tag does not start with vN.
major_of() { printf '%s\n' "$1" | sed -nE 's/^v([0-9]+)\..*/\1/p'; }

# Truncate the major-tags file up front so the caller always has a file to
# read, even when there is nothing to hold back.
[ -n "$MAJOR_TAGS_FILE" ] && : > "$MAJOR_TAGS_FILE"

[ -f "$STATE_FILE" ] || { log "ERROR: state file not found: $STATE_FILE"; exit 1; }
last_synced="$(tr -d '[:space:]' < "$STATE_FILE")"
[ -n "$last_synced" ] || { log "ERROR: state file is empty: $STATE_FILE"; exit 1; }

log "Last-synced BSC tag: $last_synced"

# Make sure the upstream remote and its tags are available. Adding the remote
# is idempotent; fetching tags is a no-op when already present.
if ! git remote get-url "$BSC_REMOTE" >/dev/null 2>&1; then
  log "Adding remote '$BSC_REMOTE' -> https://github.com/bnb-chain/bsc.git"
  git remote add "$BSC_REMOTE" https://github.com/bnb-chain/bsc.git
fi
log "Fetching tags from '$BSC_REMOTE'..."
git fetch --quiet --tags "$BSC_REMOTE"

if ! git rev-parse -q --verify "refs/tags/${last_synced}^{tag}" >/dev/null 2>&1 \
   && ! git rev-parse -q --verify "refs/tags/${last_synced}" >/dev/null 2>&1; then
  log "ERROR: last-synced tag '$last_synced' does not exist locally."
  exit 1
fi

# All version tags, ascending. Inject last_synced, version-sort+dedupe, then
# take everything that sorts strictly after it.
newer="$(
  { git tag -l 'v*' --sort=v:refname; printf '%s\n' "$last_synced"; } \
    | sort -V -u \
    | awk -v last="$last_synced" 'found{print} $0==last{found=1}'
)"

if [ "$INCLUDE_PRERELEASE" != "1" ]; then
  # Stable releases only: drop anything carrying a pre-release / feature suffix.
  newer="$(printf '%s\n' "$newer" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true)"
fi

newer="$(printf '%s\n' "$newer" | sed '/^$/d')"

if [ -z "$newer" ]; then
  log "No new tags to sync."
  exit 0
fi

# Split off major releases: from the first tag whose major version exceeds the
# last-synced major, everything is held back (those later tags would build on
# an un-merged major). The held tags go to MAJOR_TAGS_FILE for the caller to
# alert on; only the syncable prefix is printed to stdout.
last_major="$(major_of "$last_synced")"
if [ "$STOP_AT_MAJOR" = "1" ] && [ -n "$last_major" ]; then
  syncable=""
  held=""
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    tmajor="$(major_of "$tag")"
    if [ -z "$held" ] && [ -n "$tmajor" ] && [ "$tmajor" -gt "$last_major" ]; then
      held="$tag"            # first major boundary
    elif [ -n "$held" ]; then
      held="$held"$'\n'"$tag" # everything after it is held too
    else
      syncable="${syncable:+$syncable$'\n'}$tag"
    fi
  done <<< "$newer"

  if [ -n "$held" ]; then
    log "Major release(s) detected — NOT auto-syncing (held for manual review):"
    printf '%s\n' "$held" | sed 's/^/  ⚠ /' >&2
    [ -n "$MAJOR_TAGS_FILE" ] && printf '%s\n' "$held" > "$MAJOR_TAGS_FILE"
  fi
  newer="$syncable"
fi

if [ -z "$newer" ]; then
  log "No syncable (non-major) tags."
  exit 0
fi

count="$(printf '%s\n' "$newer" | wc -l | tr -d ' ')"
log "Found $count syncable tag(s):"
printf '%s\n' "$newer" | sed 's/^/  - /' >&2

printf '%s\n' "$newer"
