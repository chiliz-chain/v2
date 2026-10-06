#!/usr/bin/env bash
#
# verify-merge.sh — build + smoke-test the merged tree and append the verdict
# to the sync report (COR-176).
#
# Why this exists: the v1.7.6 sync shipped a consensus regression partly because
# the pipeline never built or tested what it merged — the report carried no
# verification section at all — and because a package whose test binary died on
# a startup panic was indistinguishable from a package with a known failing
# test. So this script distinguishes three outcomes per package — passed,
# failed tests, and could-not-run — and says UNVERIFIED loudly for the third.
# It also never aborts the sync: a broken merge still gets its draft PR,
# because a PR carrying a red verdict is more useful than no PR at all.
#
# Usage:
#   verify-merge.sh            # appends to $REPORT_MD, writes verify_status to $GITHUB_ENV
#
# Env:
#   REPORT_MD      report to append to (default: bsc-sync-report.md)
#   SMOKE_PKGS     packages to test (default: the CLAUDE.md minimum smoke set)
#   TEST_TIMEOUT   per-package go test timeout (default: 20m)
#   GITHUB_ENV     if set, verify_status=<pass|fail|unverified> is appended
#   SCRATCH_DIR    where raw logs go (default: alongside REPORT_MD)
#
# Always exits 0. Read verify_status (or the report) for the verdict.
#
set -uo pipefail

REPORT_MD="${REPORT_MD:-bsc-sync-report.md}"
SMOKE_PKGS="${SMOKE_PKGS:-./consensus/parlia/... ./core/vm/... ./params/...}"
TEST_TIMEOUT="${TEST_TIMEOUT:-20m}"
SCRATCH_DIR="${SCRATCH_DIR:-$(dirname "$REPORT_MD")}"
mkdir -p "$SCRATCH_DIR"
build_log="$SCRATCH_DIR/verify-build.log"
test_log="$SCRATCH_DIR/verify-test.log"

log() { printf '\033[0;36m[verify]\033[0m %s\n' "$*" >&2; }

# --- build ------------------------------------------------------------------
log "go build ./..."
build_ok=true
go build ./... >"$build_log" 2>&1 || build_ok=false

# --- test (only meaningful if it builds) ------------------------------------
# Explicit -timeout: the default is 10m per package and the parlia suite alone
# runs ~85s locally, so a slow runner could trip it. A timeout prints
# "panic: test timed out", which would be classified UNVERIFIED — an expensive
# flaky red on a gate whose whole value is being believed.
test_ran=false
test_ok=true
if $build_ok; then
  log "go test -timeout $TEST_TIMEOUT $SMOKE_PKGS"
  test_ran=true
  # shellcheck disable=SC2086 # SMOKE_PKGS is an intentional word list
  go test -count=1 -timeout "$TEST_TIMEOUT" $SMOKE_PKGS >"$test_log" 2>&1 || test_ok=false
fi

# --- classify ---------------------------------------------------------------
# Per package, three outcomes. A package is UNVERIFIED (no signal) when its
# test binary never finished: either a panic/fatal error appeared inside its
# output block, or it FAILed while producing no "--- FAIL:" line of its own.
#
# The panic is scoped to the FAIL line that follows it rather than applied to
# every failing package: geth tests print recovered-panic traces at column 0,
# and a global match would relabel ordinary failures elsewhere in the run as
# "no signal" — the exact confusion this script exists to remove. Note the
# panic check must stay independent of "--- FAIL:", because the real COR-37
# case printed a "--- FAIL:" line *and then* panicked.
#
# "No --- FAIL:" alone is NOT enough to call a package unverified: a package
# whose TestMain exits non-zero after its tests pass prints "PASS" and then
# "FAIL <pkg> <time>" with no per-test line, and that ran fine (verified
# against real `go test` output). So a package counts as unverified only when
# it crashed (panic/fatal), was killed by a signal (the OOM shape, which also
# prints no panic), or produced neither a per-test failure nor a PASS.
failed_tests=()
unverified_pkgs=()
panic_line=""
if $test_ran; then
  while IFS= read -r line; do
    [ -n "$line" ] && failed_tests+=("$line")
  done < <(grep -E '^[[:space:]]*--- FAIL: ' "$test_log" | sed 's/^[[:space:]]*--- FAIL: //' | cut -d' ' -f1 | sort -u)

  while IFS= read -r pkg; do
    [ -n "$pkg" ] && unverified_pkgs+=("$pkg (build failed)")
  done < <(grep -E '^FAIL[[:space:]]+[^[:space:]]+[[:space:]]+\[build failed\]' "$test_log" | awk '{print $2}' | sort -u)

  while IFS= read -r entry; do
    [ -n "$entry" ] && unverified_pkgs+=("$entry")
  done < <(awk '
      /^panic: |^fatal error: /  { sawpanic=1;  next }
      /^signal: /                { sawsignal=1; next }
      /^[[:space:]]*--- FAIL: /  { sawfail=1;   next }
      /^PASS$/                   { sawpass=1;   next }
      /^ok[[:space:]]/           { sawpanic=0; sawsignal=0; sawfail=0; sawpass=0; next }
      /^FAIL[[:space:]]+[^[:space:]]+[[:space:]]+[0-9]/ {
          if      (sawpanic)             print $2 " (test binary crashed)"
          else if (sawsignal)            print $2 " (test binary killed by a signal)"
          else if (!sawfail && !sawpass) print $2 " (reported no test results)"
          sawpanic=0; sawsignal=0; sawfail=0; sawpass=0; next
      }
    ' "$test_log" | sort -u)

  if grep -qE '^panic: |^fatal error: ' "$test_log"; then
    panic_line=$(grep -m1 -E '^panic: |^fatal error: ' "$test_log" | cut -c1-200)
    # A run that died without ever printing a package FAIL line still has no signal.
    [ ${#unverified_pkgs[@]} -eq 0 ] && unverified_pkgs+=("(run aborted before any package reported)")
  fi
fi

status=pass
if ! $build_ok; then
  status=fail
elif [ ${#unverified_pkgs[@]} -gt 0 ]; then
  status=unverified
elif ! $test_ok; then
  status=fail
fi

# --- report -----------------------------------------------------------------
{
  echo
  echo "## Verification (automated)"
  echo
  if ! $build_ok; then
    echo "🔴 **\`go build ./...\` FAILED** — the merged tree does not compile. First errors:"
    echo
    echo '```'
    grep -vE '^(go: downloading|# )' "$build_log" | head -15
    echo '```'
    echo
    echo "Tests were not run. **Do not merge until this builds.**"
  else
    echo "🟢 \`go build ./...\` passed."
    echo
    # UNVERIFIED and ordinary test failures are reported independently: a single
    # crashed package must not hide the names of genuinely failing tests elsewhere.
    if [ ${#unverified_pkgs[@]} -gt 0 ]; then
      echo "🔴 **UNVERIFIED — a test binary crashed, was killed, or reported nothing, so these packages are NOT known-good:**"
      echo
      for p in ${unverified_pkgs[@]+"${unverified_pkgs[@]}"}; do echo "- \`$p\`"; done
      [ -n "$panic_line" ] && { echo; echo "First crash: \`$panic_line\`"; }
      echo
      echo "⚠️ Treat this as *no signal at all* for those packages — not as a known failure. Everything they cover is untested in this report."
      echo
    fi
    if ! $test_ok; then
      echo "🔴 **Smoke tests FAILED** (\`$SMOKE_PKGS\`):"
      echo
      if [ ${#failed_tests[@]} -gt 0 ]; then
        for t in ${failed_tests[@]+"${failed_tests[@]}"}; do echo "- \`$t\`"; done
      else
        # e.g. "FAIL <pkg> [setup failed]", or a go invocation error: the run is
        # red but named no test. Fall back to the package list, then to the log.
        # Exclude packages already itemised under UNVERIFIED above, so a
        # crashed/killed package is not reported twice as two problems.
        pkgs=$(grep -E '^FAIL[[:space:]]' "$test_log" | awk '{print $2}' | sort -u)
        for u in ${unverified_pkgs[@]+"${unverified_pkgs[@]}"}; do
          pkgs=$(printf '%s\n' "$pkgs" | grep -vxF "${u%% (*}" || true)
        done
        if [ -n "$pkgs" ]; then
          while IFS= read -r pk; do [ -n "$pk" ] && echo "- \`$pk\` (failed without naming a test)"; done <<<"$pkgs"
        elif [ ${#unverified_pkgs[@]} -gt 0 ]; then
          echo "- no additional named failures — see the UNVERIFIED list above"
        else
          echo "- unnamed failure — see \`verify-test.log\` in the artifact"
        fi
      fi
      echo
      echo "Cross-check against the known pre-existing failures in CLAUDE.md before assuming a regression."
    elif [ ${#unverified_pkgs[@]} -eq 0 ]; then
      echo "🟢 Smoke set passed (\`$SMOKE_PKGS\`)."
    fi
    echo
    echo "> Scope: this is the CLAUDE.md **minimum** smoke set — a build plus three packages. It does **not** exercise multi-node block production, which is where the COR-37 consensus regression lived. A merge touching \`Prepare\`/\`Seal\`/\`snapshot()\`/worker ordering still requires the multi-validator devnet run. Note also that this job checks out with \`submodules: false\`, so \`genesis/\` is empty and the genesis tooling (\`genesis/create-genesis.go\`, which imports \`common/systemcontract\` and \`triedb\`) is **not** covered by the build."
  fi
  echo
  echo "_Raw output: \`verify-build.log\` / \`verify-test.log\` in the \`bsc-sync-agent-logs\` artifact._"
} >>"$REPORT_MD"

# --- signal -----------------------------------------------------------------
[ -n "${GITHUB_ENV:-}" ] && echo "verify_status=$status" >>"$GITHUB_ENV"
case "$status" in
  pass)       log "verification passed" ;;
  unverified) echo "::error::Sync verification UNVERIFIED — a test binary never completed; affected packages have no signal. See the PR report." ;;
  fail)       echo "::error::Sync verification FAILED (build or smoke tests). See the PR report." ;;
esac
exit 0
