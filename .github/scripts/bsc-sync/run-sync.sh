#!/usr/bin/env bash
#
# run-sync.sh — merge upstream BSC tags one by one, delegating conflict
# resolution to a headless Claude Code agent, and emit a report.
#
# Usage:
#   run-sync.sh <tag> [<tag> ...]
#
# The tags must be passed in ascending (merge) order — see detect-new-tags.sh.
# This script assumes it runs on a dedicated sync branch that the caller has
# already checked out; it commits each merge onto the current HEAD.
#
# Per tag the merge has three outcomes:
#   clean        -> committed automatically
#   conflicts    -> Claude agent resolves; merge is committed even if some
#                   files are left with raw markers (those surface in the PR)
#   unrecoverable -> merge aborted, remaining tags not attempted
#
# Outputs (paths overridable via env):
#   REPORT_MD    human-readable markdown report  (default: bsc-sync-report.md)
#   OUTPUTS_ENV  key=value summary for the workflow (default: bsc-sync.env)
#
# Agent invocation:
#   SKIP_AGENT=1     never call the agent; treat every conflict as 🔴 and stop
#   CLAUDE_BIN       agent binary (default: claude)
#   CLAUDE_MODEL     optional model override passed as --model
# If the agent binary is missing, behaviour is the same as SKIP_AGENT=1.
#
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMPT_TEMPLATE="$SCRIPT_DIR/resolve-prompt.md"
IMPACT_TEMPLATE="$SCRIPT_DIR/impact-prompt.md"

REPORT_MD="${REPORT_MD:-$REPO_ROOT/bsc-sync-report.md}"
OUTPUTS_ENV="${OUTPUTS_ENV:-$REPO_ROOT/bsc-sync.env}"
AGENT_REPORT_DIR="${AGENT_REPORT_DIR:-$REPO_ROOT/.bsc-sync-agent}"
CLAUDE_BIN="${CLAUDE_BIN:-claude}"

[ "$#" -ge 1 ] || { echo "usage: run-sync.sh <tag> [<tag> ...]" >&2; exit 2; }
TAGS=("$@")

mkdir -p "$AGENT_REPORT_DIR"

# --- counters & report state ------------------------------------------------
n_clean=0; n_high=0; n_low=0; n_unresolved=0; n_skipped=0; n_failed=0
n_agent_error=0           # tags where the agent failed to run (API error, etc.)
n_unverified=0            # tags committed but with no machine-readable agent report
agent_hard_error=false    # a runtime agent failure occurred (fatal for the run)
declare -a ROWS=()        # markdown table rows
declare -a NOTES=()       # free-form notes for the PR body
synced_tags=()            # tags actually committed (clean or resolved)
errored_tags=()           # tags the agent failed to resolve at runtime
last_committed=""
base_commit="$(git rev-parse HEAD)"   # our tree before any merge (for the impact report)

log()  { printf '\033[0;36m[bsc-sync]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[0;33m[bsc-sync]\033[0m %s\n' "$*" >&2; }

agent_available() {
  [ -z "${SKIP_AGENT:-}" ] && command -v "$CLAUDE_BIN" >/dev/null 2>&1
}

# Build the per-tag prompt by substituting the conflicted file list and paths
# into the template.
build_prompt() {
  local tag="$1" report_path="$2" files="$3"
  local tpl; tpl="$(cat "$PROMPT_TEMPLATE")"
  tpl="${tpl//'{{TAG}}'/$tag}"
  tpl="${tpl//'{{REPORT_PATH}}'/$report_path}"
  tpl="${tpl//'{{FILES}}'/$files}"
  printf '%s' "$tpl"
}

# Files still carrying conflict markers (i.e. 🔴 left for humans).
files_with_markers() {
  git -c core.quotepath=false diff --name-only --diff-filter=U 2>/dev/null
  # also catch markers committed into otherwise-staged files
  git grep -lE '^(<{7}|={7}|>{7})( |$)' -- . 2>/dev/null || true
}

invoke_agent() {
  local tag="$1" report_path="$2"
  local conflicted; conflicted="$(git diff --name-only --diff-filter=U | sed 's/^/- /')"
  local prompt; prompt="$(build_prompt "$tag" "$report_path" "$conflicted")"
  local model_args=(); [ -n "${CLAUDE_MODEL:-}" ] && model_args=(--model "$CLAUDE_MODEL")
  log "Invoking agent ($CLAUDE_BIN) to resolve $(printf '%s' "$conflicted" | grep -c . ) file(s)..."
  # ${arr[@]+"${arr[@]}"} keeps empty-array expansion safe under `set -u` (bash 3.2).
  printf '%s\n' "$prompt" \
    | "$CLAUDE_BIN" --print --dangerously-skip-permissions ${model_args[@]+"${model_args[@]}"} \
        >"$AGENT_REPORT_DIR/agent-$tag.log" 2>&1
}

# Build the release-impact prompt: base ref, synced tags, output path.
build_impact_prompt() {
  local base="$1" tags="$2" impact_path="$3"
  local tpl; tpl="$(cat "$IMPACT_TEMPLATE")"
  tpl="${tpl//'{{BASE}}'/$base}"
  tpl="${tpl//'{{TAGS}}'/$tags}"
  tpl="${tpl//'{{IMPACT_PATH}}'/$impact_path}"
  printf '%s' "$tpl"
}

invoke_impact_agent() {
  local base="$1" tags="$2" impact_path="$3"
  local prompt; prompt="$(build_impact_prompt "$base" "$tags" "$impact_path")"
  local model_args=(); [ -n "${CLAUDE_MODEL:-}" ] && model_args=(--model "$CLAUDE_MODEL")
  log "Invoking agent ($CLAUDE_BIN) to write the release-impact report..."
  printf '%s\n' "$prompt" \
    | "$CLAUDE_BIN" --print --dangerously-skip-permissions ${model_args[@]+"${model_args[@]}"} \
        >"$AGENT_REPORT_DIR/impact.log" 2>&1
}

# --- main loop --------------------------------------------------------------
aborted=false
for tag in "${TAGS[@]}"; do
  if $aborted; then
    log "Skipping $tag (a previous tag could not be merged)."
    ROWS+=("| \`$tag\` | ⏭️ not attempted | — | prior tag blocked the chain |")
    n_skipped=$((n_skipped+1))
    continue
  fi

  log "Merging $tag ..."
  if git merge --no-ff --no-edit "$tag" >/dev/null 2>"$AGENT_REPORT_DIR/merge-$tag.err"; then
    log "$tag merged cleanly."
    ROWS+=("| \`$tag\` | ✅ clean | 0 | merged without conflicts |")
    n_clean=$((n_clean+1))
    synced_tags+=("$tag"); last_committed="$tag"
    continue
  fi

  # merge returned non-zero: conflicts, or an error that left no merge state.
  if [ -z "$(git ls-files --unmerged)" ]; then
    warn "$tag could not be merged (no conflict state — unrecoverable)."
    git merge --abort 2>/dev/null || true
    ROWS+=("| \`$tag\` | ❌ unrecoverable | — | merge failed to start; see merge-$tag.err |")
    NOTES+=("**$tag** failed to merge (unrecoverable). Remaining tags were not attempted.")
    n_failed=$((n_failed+1))
    aborted=true
    continue
  fi

  conflict_count="$(git diff --name-only --diff-filter=U | grep -c . || true)"
  warn "$tag has conflicts in $conflict_count file(s)."

  if ! agent_available; then
    warn "Agent unavailable (SKIP_AGENT set or '$CLAUDE_BIN' not found). Aborting at $tag."
    git merge --abort 2>/dev/null || true
    ROWS+=("| \`$tag\` | 🔴 conflicts (agent unavailable) | $conflict_count | no API key / agent; needs human merge |")
    NOTES+=("**$tag** has $conflict_count conflicted file(s) but no agent was available to resolve them. Remaining tags were not attempted.")
    n_unresolved=$((n_unresolved+conflict_count))
    aborted=true
    continue
  fi

  agent_report="$AGENT_REPORT_DIR/report-$tag.json"
  rm -f "$agent_report"
  if ! invoke_agent "$tag" "$agent_report"; then
    # The agent FAILED to run (e.g. Claude API error / usage limit). The
    # conflicts are NOT resolved, and the agent almost certainly won't recover
    # within this run. Abort BEFORE staging: committing the untouched conflict
    # tree here would look like a rosy "resolved" result (the real bug behind
    # the misleading green Slack message). Do not commit, do not attempt
    # further tags.
    err_line="$(tail -n 1 "$AGENT_REPORT_DIR/agent-$tag.log" 2>/dev/null | tr -d '\r' | cut -c1-200)"
    warn "Agent invocation FAILED for $tag — aborting run (see agent-$tag.log)."
    git merge --abort 2>/dev/null || true
    ROWS+=("| \`$tag\` | 🛑 agent error | $conflict_count | agent failed to run — NOT resolved; see agent-$tag.log |")
    NOTES+=("**$tag** — the resolution agent **failed to run** (\`${err_line:-see agent-$tag.log}\`). Its $conflict_count conflict(s) were not resolved and the tag was **not committed**. Remaining tags were skipped. Re-run once the agent is available again.")
    n_agent_error=$((n_agent_error+1))
    errored_tags+=("$tag")
    agent_hard_error=true
    aborted=true
    continue
  fi

  # Stage whatever the agent produced.
  git add -A

  # Tally per-file confidence from the agent report (best effort).
  t_high=0; t_low=0; t_unres=0; unverified=false
  if [ -f "$agent_report" ] && command -v jq >/dev/null 2>&1 && jq -e . "$agent_report" >/dev/null 2>&1; then
    t_high=$(jq '[.resolutions[]?|select(.confidence=="high")]|length' "$agent_report")
    t_low=$(jq '[.resolutions[]?|select(.confidence=="low")]|length' "$agent_report")
    t_unres=$(jq '[.resolutions[]?|select(.confidence=="unresolved")]|length' "$agent_report")
  else
    # Agent returned success but produced no machine-readable report — we
    # cannot confirm the resolutions. Don't report a silent 🟢; flag for review.
    warn "No parseable agent report for $tag; resolutions unverified (relying on marker scan)."
    unverified=true
  fi

  # Authoritative check: any conflict markers actually left in tracked files.
  remaining_markers="$(files_with_markers | sort -u | sed '/^$/d')"
  marker_files=0; [ -n "$remaining_markers" ] && marker_files="$(printf '%s\n' "$remaining_markers" | grep -c .)"
  if [ "$marker_files" -gt 0 ]; then
    # markers win the classification regardless of what the report claimed
    t_unres="$marker_files"
    NOTES+=("**$tag** — $marker_files file(s) left with raw conflict markers (🔴), commit them and resolve by hand:")
    while IFS= read -r f; do [ -n "$f" ] && NOTES+=("  - \`$f\`"); done <<<"$remaining_markers"
  fi

  # If the agent gave no report and left no detectable markers, we still can't
  # be sure it resolved everything correctly (e.g. non-textual conflicts) —
  # surface it as needs-review rather than a silent 🟢.
  if [ "$unverified" = true ] && [ "$t_unres" -eq 0 ]; then
    n_unverified=$((n_unverified+1))
    NOTES+=("**$tag** — agent produced no machine-readable report; its $conflict_count resolution(s) are **unverified**. Please review the diff for this tag.")
  fi

  n_high=$((n_high+t_high)); n_low=$((n_low+t_low)); n_unresolved=$((n_unresolved+t_unres))

  status_emoji="🟢"
  { [ "$t_low" -gt 0 ] || [ "$unverified" = true ]; } && status_emoji="🟡"
  [ "$t_unres" -gt 0 ] && status_emoji="🔴"
  ROWS+=("| \`$tag\` | $status_emoji agent-resolved | $conflict_count | 🟢$t_high / 🟡$t_low / 🔴$t_unres |")

  # Commit the merge. We commit even with 🔴 markers so they appear in the
  # PR diff for mandatory human resolution (the safety property from COR-37).
  git commit --no-edit >/dev/null 2>&1 || git commit --no-edit -m "merge bsc $tag (conflicts resolved by agent)" >/dev/null
  synced_tags+=("$tag"); last_committed="$tag"
  log "$tag committed (🟢$t_high 🟡$t_low 🔴$t_unres)."
done

# --- write reports ----------------------------------------------------------
{
  echo "## Upstream BSC sync report"
  echo
  # Loud banner when the run did not complete, so nobody mistakes a partial
  # sync for a clean one.
  if [ "$agent_hard_error" = true ]; then
    echo "> 🛑 **This sync did NOT complete.** The resolution agent failed to run"
    echo "> (e.g. Claude API error / usage limit) on: $(printf '\`%s\` ' "${errored_tags[@]}")."
    echo "> Those tag(s) and any after them were **not** synced. Re-run once the"
    echo "> agent is available; only the tags listed as synced below were committed."
    echo
  elif [ "$n_failed" -gt 0 ]; then
    echo "> ⚠️ **This sync did not complete** — one or more tags could not be merged."
    echo "> See the table below; remaining tags were not attempted."
    echo
  fi
  echo "Attempted tags (in order): $(printf '\`%s\` ' "${TAGS[@]}")"
  echo
  echo "| Tag | Result | Conflicts | Detail |"
  echo "|-----|--------|-----------|--------|"
  for r in "${ROWS[@]}"; do echo "$r"; done
  echo
  echo "**Totals:** 🟢 $n_high high-confidence · 🟡 $n_low needs-review · ❔ $n_unverified unverified · 🔴 $n_unresolved unresolved · ✅ $n_clean clean · 🛑 $n_agent_error agent-errored · ❌ $n_failed unrecoverable · ⏭️ $n_skipped not attempted"
  echo
  if [ "$n_low" -gt 0 ] || [ "$n_unresolved" -gt 0 ] || [ "$n_agent_error" -gt 0 ] || [ "$n_failed" -gt 0 ] || [ "$n_unverified" -gt 0 ]; then
    echo "### Items needing human attention"
    echo
    if [ "$n_low" -gt 0 ]; then
      echo "- 🟡 Search the diff for \`TODO: Claude flagged, please review\` — $n_low low-confidence resolution(s) to audit."
    fi
    for note in "${NOTES[@]}"; do echo "- $note"; done
    echo
  fi
  echo "_Per-tag agent logs and JSON reports are attached to the workflow run as artifacts (\`.bsc-sync-agent/\`)._"
} > "$REPORT_MD"

# --- release-impact report (prepended, so it leads the PR body) -------------
# A one-shot agent pass over the whole merged range: what the release does,
# which config/constants moved, and whether any Chiliz invariant was touched.
if [ "${#synced_tags[@]}" -gt 0 ]; then
  impact_md="$AGENT_REPORT_DIR/impact.md"
  rm -f "$impact_md"
  # Skip the impact agent if the agent already hard-errored this run — it would
  # just fail again with the same API error.
  if agent_available && [ "$agent_hard_error" != true ]; then
    if ! invoke_impact_agent "$base_commit" "${synced_tags[*]}" "$impact_md"; then
      warn "Release-impact agent invocation failed (see impact.log)."
    fi
  fi
  if [ -s "$impact_md" ]; then
    log "Release-impact report generated; prepending to $REPORT_MD."
    cat "$impact_md" "$REPORT_MD" > "$REPORT_MD.tmp" && mv "$REPORT_MD.tmp" "$REPORT_MD"
  else
    # Be honest in the report when the summary could not be produced.
    note="_Release-impact summary not generated"
    if [ "$agent_hard_error" = true ]; then note="$note (skipped — agent errored during the sync)."
    elif agent_available; then note="$note (agent produced no output — see impact.log)."
    else note="$note (agent unavailable: no API key)."; fi
    note="$note Review the diff manually._"
    printf '%s\n\n%s\n' "$note" "$(cat "$REPORT_MD")" > "$REPORT_MD"
  fi
fi

# Decide the highest-priority review state for the Slack message.
review_state="clean"
if [ "$n_low" -gt 0 ] || [ "$n_unverified" -gt 0 ]; then review_state="needs-review"; fi
if [ "$n_unresolved" -gt 0 ] || [ "$n_failed" -gt 0 ]; then review_state="needs-human"; fi
if [ "$agent_hard_error" = true ]; then review_state="agent-error"; fi

{
  echo "synced_count=${#synced_tags[@]}"
  echo "synced_tags=${synced_tags[*]:-}"
  echo "last_committed=${last_committed}"
  echo "clean=$n_clean"
  echo "high=$n_high"
  echo "low=$n_low"
  echo "unresolved=$n_unresolved"
  echo "unverified=$n_unverified"
  echo "failed=$n_failed"
  echo "agent_error=$n_agent_error"
  echo "errored_tags=${errored_tags[*]:-}"
  echo "skipped=$n_skipped"
  echo "review_state=$review_state"
  echo "has_changes=$([ "${#synced_tags[@]}" -gt 0 ] && echo true || echo false)"
} > "$OUTPUTS_ENV"

log "Report written to $REPORT_MD"
log "Outputs written to $OUTPUTS_ENV"
cat "$REPORT_MD" >&2
