#!/usr/bin/env bash
# Run every native Go fuzz target (func Fuzz*) in the given packages, one at a
# time (the Go toolchain fuzzes a single package per invocation), each for
# $FUZZTIME. Discovery is by `go test -list`, so a new target is picked up the
# moment it lands on develop and nothing here needs editing.
#
# Usage: run-fuzz.sh [--require-complete|--write-baseline] <pkg>...
#        env: FUZZTIME (default 2m), SUMMARY (file), BASELINE (file)
#
# Exit status is non-zero if any target failed, and also if no target was found
# at all — a campaign that fuzzed nothing is a failure, not a pass. A failing
# input is written by the toolchain under <pkg>/testdata/fuzz/<Target>/<hash>;
# the workflow uploads those files as an artifact so the crasher can be
# reproduced with
#   go test <pkg> -run '^<Target>$'
# and, once fixed, committed as a regression seed.
set -uo pipefail

FUZZTIME="${FUZZTIME:-2m}"
SUMMARY="${SUMMARY:-/dev/null}"
BASELINE="${BASELINE:-$(dirname "$0")/expected-targets.txt}"
failed=0
total=0

# --write-baseline regenerates expected-targets.txt from what the given packages
# currently list, for use after adding targets. It fuzzes nothing.
#
# --require-complete additionally asserts that every package the baseline records
# a non-zero minimum for was actually in the package list. Only the caller knows
# whether the list is meant to be the whole set, which is why this is a flag: the
# weekly run over DEFAULT_PACKAGES passes it, a workflow_dispatch naming a subset
# deliberately does not.
write_baseline=0
require_complete=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --write-baseline)   write_baseline=1; shift ;;
    --require-complete) require_complete=1; shift ;;
    --) shift; break ;;
    -*) printf '::error::run-fuzz.sh: unknown flag %s\n' "$1"; exit 1 ;;
    *) break ;;
  esac
done

# expected_for <pkg> -> the package's recorded minimum, 0 when unlisted.
expected_for() {
  [ -f "$BASELINE" ] || { printf '0'; return; }
  awk -v p="$1" '$1 == p { print $2; found = 1 } END { if (!found) print 0 }' "$BASELINE" | head -1
}

# No packages at all is a caller bug, not an empty campaign: an unset/empty
# DEFAULT_PACKAGES, or a whitespace-only dispatch input, word-splits to zero
# arguments here and would otherwise sail through the loop and exit 0.
if [ "$#" -eq 0 ]; then
  printf '::error::run-fuzz.sh: no packages given\n'
  exit 1
fi

if [ "$write_baseline" -eq 1 ]; then
  baseline_tmp=$(mktemp)
else
  printf '| Package | Target | Execs | Result |\n| -- | -- | --: | -- |\n' >> "$SUMMARY"
fi

for pkg in "$@"; do
  # `go test -list` prints one name per line followed by "ok <pkg> ...". A
  # package that fails to compile (or to list) must fail the run, not pass as
  # "no fuzz targets": keep its output and check the exit status.
  listing=$(mktemp)
  if ! go test -list '^Fuzz' "$pkg" >"$listing" 2>&1; then
    printf '::error::%s: go test -list failed\n' "$pkg"
    cat "$listing"
    rm -f "$listing"
    failed=$((failed + 1))
    printf '| `%s` | (listing) | 0 | **FAIL** |\n' "$pkg" >> "$SUMMARY"
    continue
  fi
  targets=$(grep -E '^Fuzz' "$listing" || true)
  rm -f "$listing"
  count=$(printf '%s' "$targets" | grep -cE '^Fuzz' || true)
  if [ "$write_baseline" -eq 1 ]; then
    printf '%s %s\n' "$pkg" "$count" >> "$baseline_tmp"
    continue
  fi
  # The ratchet. A package that used to carry targets and now carries fewer means
  # discovery broke — renamed targets, a dropped package, a bad merge — not a
  # clean campaign. See expected-targets.txt.
  want=$(expected_for "$pkg")
  if [ "$count" -lt "$want" ]; then
    printf '::error::%s: %d fuzz target(s), expected at least %d — discovery is broken (renamed targets? package dropped in an upstream sync?). If the drop is intentional, refresh .github/scripts/fuzz/expected-targets.txt in the same PR.\n' "$pkg" "$count" "$want"
    printf '| `%s` | (discovery) | %d of %d | **FAIL** |\n' "$pkg" "$count" "$want" >> "$SUMMARY"
    failed=$((failed + 1))
    continue
  fi
  if [ "$count" -gt "$want" ]; then
    printf '::notice::%s: %d fuzz target(s), baseline records %d — run .github/scripts/fuzz/run-fuzz.sh --write-baseline to refresh\n' "$pkg" "$count" "$want"
  fi
  if [ -z "$targets" ]; then
    printf '::notice::%s: no fuzz targets\n' "$pkg"
    continue
  fi
  for t in $targets; do
    total=$((total + 1))
    printf '::group::%s %s (%s)\n' "$pkg" "$t" "$FUZZTIME"
    log=$(mktemp)
    if go test "$pkg" -run '^$' -fuzz "^${t}\$" -fuzztime "$FUZZTIME" 2>&1 | tee "$log"; then
      result="ok"
    else
      result="**FAIL**"
      failed=$((failed + 1))
      printf '::error::%s %s failed; failing input under %s/testdata/fuzz/%s\n' "$pkg" "$t" "$pkg" "$t"
    fi
    execs=$(grep -oE 'execs: [0-9]+' "$log" | tail -1 | grep -oE '[0-9]+' || echo 0)
    printf '| `%s` | `%s` | %s | %s |\n' "$pkg" "$t" "$execs" "$result" >> "$SUMMARY"
    rm -f "$log"
    printf '::endgroup::\n'
  done
done

if [ "$write_baseline" -eq 1 ]; then
  {
    sed -n '1,/^# <package> <minimum targets>$/p' "$BASELINE" 2>/dev/null || true
    sort "$baseline_tmp"
  } > "$BASELINE.new"
  mv "$BASELINE.new" "$BASELINE"
  rm -f "$baseline_tmp"
  printf 'wrote %s\n' "$BASELINE"
  exit 0
fi

# The per-package ratchet inside the loop is keyed by the package being fuzzed,
# so it is silent about a package that is not in the list at all: an upstream
# sync that drops ./consensus/parlia from DEFAULT_PACKAGES leaves the remaining
# packages each meeting their minimum, failed at 0 and the run green — with
# every one of that package's targets unfuzzed. The two failure modes are
# different and both need covering:
#
#   targets renamed away from Fuzz*, package still listed -> count < minimum
#   package dropped from the list entirely                -> this check
if [ "$require_complete" -eq 1 ] && [ -f "$BASELINE" ]; then
  while read -r bpkg bmin _rest; do
    case "$bpkg" in ''|\#*) continue ;; esac
    [ "${bmin:-0}" -gt 0 ] 2>/dev/null || continue
    listed=0
    for pkg in "$@"; do
      [ "$pkg" = "$bpkg" ] && { listed=1; break; }
    done
    if [ "$listed" -eq 0 ]; then
      printf '::error::%s is recorded in %s with %s fuzz target(s) but was not in the package list — its targets did not run. If the removal is intentional, drop its line from the baseline in the same PR.\n' "$bpkg" "$BASELINE" "$bmin"
      printf '| `%s` | (not fuzzed) | 0 of %s | **FAIL** |\n' "$bpkg" "$bmin" >> "$SUMMARY"
      failed=$((failed + 1))
    fi
  done < "$BASELINE"
fi

printf '\n%d target(s), %d failed, %s each\n' "$total" "$failed" "$FUZZTIME" >> "$SUMMARY"

# Discovery is implicit, so its failure is silent: every package reporting "no
# fuzz targets" looks exactly like a clean campaign. It is not — it means the
# targets were renamed away from Fuzz*, moved out of the package list, or the
# list itself never reached us. A green weekly run that fuzzed nothing would
# hide that until the crasher we never looked for reaches a validator.
if [ "$total" -eq 0 ] && [ "$failed" -eq 0 ]; then
  printf '::error::no fuzz targets found in: %s\n' "$*"
  printf '\n**No fuzz targets found** — nothing was fuzzed; discovery is broken.\n' >> "$SUMMARY"
  exit 1
fi

[ "$failed" -eq 0 ]
