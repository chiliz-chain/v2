#!/usr/bin/env bash
# Post one message to Slack as the shared Chiliz Chain CI bot.
#
# Usage: slack-notify.sh --channel <#chan> --username <name> --icon <:emoji:> [options] <text>
#   --thread-ts <ts>   post as a reply in that thread
#   --blocks <json>    Block Kit array; `text` is still sent as the notification fallback
#   env SLACK_CI_BOT_TOKEN   required; when unset the script no-ops and exits 0.
#                            Deliberately NOT named SLACK_BOT_TOKEN: an org-level
#                            secret of that name exists, and a repo secret
#                            shadowing it is invisible to anyone without
#                            org-admin. If the shadow were ever removed the
#                            workflows would silently fall back to a different
#                            Slack app, and if that app lacks chat:write.customize
#                            the per-workflow identity stops applying with no
#                            error at all.
#   env SLACK_API_URL     override the endpoint (tests only)
#
# On success the message ts is printed on stdout, so a caller can thread replies
# under it. On a Slack rejection the script exits 1 with a ::error:: annotation;
# callers are expected to set continue-on-error so a notification problem
# annotates the run without failing the work the run actually did.
#
# Why this exists as a script rather than inline in each workflow: three call
# sites (fuzz, the two BSC-sync steps) share one non-obvious trap, below.
set -uo pipefail

API_URL="${SLACK_API_URL:-https://slack.com/api/chat.postMessage}"
channel="" username="" icon="" thread_ts="" blocks="" text=""

# `shift 2` with only one argument left FAILS and shifts nothing. Without -e
# that is not fatal, it is worse: the loop sees the same argv forever and the
# script hangs instead of reporting bad usage. So every value-taking flag
# asserts it has a value first.
need_value() {
  [ "$#" -ge 2 ] || {
    printf '::error::slack-notify.sh: %s requires a value\n' "$1" >&2
    exit 2
  }
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --channel)   need_value "$@"; channel="$2"; shift 2 ;;
    --username)  need_value "$@"; username="$2"; shift 2 ;;
    --icon)      need_value "$@"; icon="$2"; shift 2 ;;
    --thread-ts) need_value "$@"; thread_ts="$2"; shift 2 ;;
    --blocks)    need_value "$@"; blocks="$2"; shift 2 ;;
    --) shift; break ;;
    # Only `--long` shapes are treated as flags. A plain `-` prefix is left to
    # the text: a message opening with "- bullet" or a dash is ordinary content,
    # and rejecting it here would exit 2 — which, under continue-on-error in
    # bsc-upstream-sync.yml, is dropped in silence. Callers also pass `--`
    # explicitly, so this is the second of two guards, not the only one.
    --*) printf '::error::slack-notify.sh: unknown flag %s\n' "$1" >&2; exit 2 ;;
    *) break ;;
  esac
done
text="${1:-}"

if [ -z "$channel" ] || [ -z "$text" ]; then
  printf '::error::slack-notify.sh: --channel and a text argument are required\n' >&2
  exit 2
fi

# A missing token is a configuration state, not an error: a fork, or a repo the
# secret was never added to, should still run its workflows.
if [ -z "${SLACK_CI_BOT_TOKEN:-}" ]; then
  printf 'SLACK_CI_BOT_TOKEN not set — skipping Slack notification.\n'
  exit 0
fi

# Reject bad Block Kit here rather than letting jq fail below: --argjson on
# malformed JSON exits non-zero, and without -e that leaves payload empty and
# POSTs an empty body, so Slack answers with a rejection that blames Slack for
# what is the caller's own JSON.
if [ -n "$blocks" ] && ! printf '%s' "$blocks" | jq -e . >/dev/null 2>&1; then
  printf '::error::slack-notify.sh: --blocks is not valid JSON\n' >&2
  exit 2
fi

# jq builds the payload so nothing in the message body can break the JSON.
if ! payload=$(jq -n \
  --arg c "$channel" --arg t "$text" --arg u "$username" \
  --arg i "$icon" --arg th "$thread_ts" --argjson bl "${blocks:-null}" \
  '{channel: $c, text: $t}
   + (if $u  != "" then {username: $u}    else {} end)
   + (if $i  != "" then {icon_emoji: $i}  else {} end)
   + (if $th != "" then {thread_ts: $th}  else {} end)
   + (if $bl != null then {blocks: $bl}   else {} end)'); then
  printf '::error::slack-notify.sh: could not build the request payload\n' >&2
  exit 2
fi

# Capture curl's own status. Without this a DNS failure or a --max-time timeout
# leaves resp empty, and since `jq -r '.ok // false'` on empty input prints
# nothing (exit 0), the rejection branch below reports "Slack rejected the
# message:" with a blank reason — sending the reader after the token, the scopes
# and the channel when the fault was the network. -sS keeps curl quiet except
# for that error text, which is what we want to quote.
if ! resp=$(curl -sS --max-time 30 -X POST "$API_URL" \
  -H "Authorization: Bearer ${SLACK_CI_BOT_TOKEN}" \
  -H 'Content-type: application/json; charset=utf-8' \
  --data "$payload" 2>&1); then
  printf '::error::could not reach Slack at %s: %s\n' "$API_URL" "$resp" >&2
  exit 1
fi

# THE TRAP: chat.postMessage answers HTTP 200 even when it refuses the message.
# Success is the `ok` field in the body, never the status code — which is also
# why this curl is -sS and not -sSf (-f could never fire). Same shape as two
# bugs already found in this area: a step summary written to /dev/null via a
# non-existent github.step_summary context, and incoming-webhook `username`
# overrides being silently ignored. Each read correctly and quietly did nothing.
ok=$(printf '%s' "$resp" | jq -r '.ok // false' 2>/dev/null)
if [ -z "$ok" ]; then
  printf '::error::Slack returned a body that is not JSON: %s\n' "${resp:-<empty>}" >&2
  exit 1
fi
if [ "$ok" != "true" ]; then
  printf '::error::Slack rejected the message: %s\n' \
    "$(printf '%s' "$resp" | jq -r '.error // "unknown error"')" >&2
  exit 1
fi

printf '%s\n' "$(printf '%s' "$resp" | jq -r '.ts')"
