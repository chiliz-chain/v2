# Automated upstream BSC sync (COR-37)

Chiliz Chain (CC2) is a fork of [bnb-chain/bsc](https://github.com/bnb-chain/bsc).
We periodically merge upstream BSC release tags to pick up bug fixes,
performance work, and protocol upgrades while preserving Chiliz-specific
customisations (see [`CLAUDE.md`](../CLAUDE.md)).

This used to be an entirely manual chore. The
[`BSC Upstream Sync`](../.github/workflows/bsc-upstream-sync.yml) workflow now
automates the mechanical parts — including a first pass at conflict resolution
by a headless Claude Code agent — and hands the result to a human as a **draft
PR**. Claude never merges to `develop`.

## What it does

Every Monday at 06:00 UTC (and on manual dispatch):

1. **Detect** — compares the last-synced tag (`.github/bsc-sync/last-synced-tag`)
   against all tags on `bnb-chain/bsc` and lists what's new, in ascending order.
2. **Merge tag by tag** — runs `git merge --no-ff` for each new tag:
   - ✅ **clean** → committed automatically;
   - ⚠️ **conflicts** → handed to the Claude agent (step 3);
   - ❌ **unrecoverable** (merge can't even start) → aborted, remaining tags
     not attempted, flagged in the report.

   **Major releases are alert-only.** A tag whose *major version* increases
   relative to the last-synced tag (e.g. `v1.7.3 → v2.0.0`) is too significant
   to merge by AI. Detection stops the syncable list before the first such tag
   (and every tag after it, since they would build on an un-merged major);
   minor/patch tags *before* the boundary still auto-sync. The held major(s)
   are **not merged** — they only trigger a dedicated Slack alert so a human
   runs that upgrade manually. The gate is bypassed for explicit manual tag
   lists (see *Manual runs*).
3. **Resolve** — on conflicts, a headless `claude --print` agent reads
   `CLAUDE.md` (the fork's invariants / safe-to-override / grey-area policy),
   the conflict hunks, and both diffs, then writes resolved files.
4. **Classify** — every resolution is annotated:
   - 🟢 **high** — clean, committed as-is;
   - 🟡 **low** — committed but marked inline with
     `TODO: Claude flagged, please review`;
   - 🔴 **unresolved** — raw conflict markers left in place and committed so
     they surface in the PR diff for mandatory human resolution.
5. **Release-impact report** — a one-shot agent pass over the whole merged
   range (pre-sync base → `HEAD`) writes a **Release impact** summary that
   leads the PR body: what the release changes, a config/parameter diff
   (upstream value vs. our value after the sync) focused on the `CLAUDE.md`
   invariant areas, an explicit invariant-impact callout, and noteworthy
   *clean-merged* changes a reviewer should still eyeball. Without an API key
   this section is skipped with an honest note.
6. **Draft PR** — once all tags are processed, a draft PR is opened against
   `develop` with the impact summary, the tag list, and a conflict-resolution
   table.
7. **Slack** — a message is posted to `#bu-chiliz-core-squad` with the tags
   synced, the 🟢/🟡/🔴 counts, a link to the PR, and — if any were detected —
   a major-release alert listing the versions that were **not** auto-merged.
8. **Human review** — a teammate audits the resolutions (especially 🟡),
   fixes any 🔴, and promotes the PR to *ready* when satisfied.

## Safety properties

- Claude **never pushes to `develop`** — always a draft PR requiring approval.
- **Major releases are never auto-merged** — they are alert-only; a human runs
  the upgrade manually.
- Every resolution is **visible in the PR diff** — nothing is applied silently.
- Low-confidence resolutions are **flagged inline** in the code.
- `CLAUDE.md` is **maintained by the team**; resolution quality tracks how well
  it documents Chiliz-specific logic. Keep its "Historical conflict patterns"
  log current after every sync.

## Configuration

| Kind | Name | Purpose |
|------|------|---------|
| Secret | `ANTHROPIC_API_KEY` | Enables the conflict-resolution agent. **When absent, clean merges still flow but conflicting tags are reported for manual merge instead of being auto-resolved.** |
| Secret | `SLACK_WEBHOOK_URL` | Incoming webhook for `#bu-chiliz-core-squad`. If unset, the Slack step is skipped (non-fatal). |
| Variable | `BSC_SYNC_CLAUDE_MODEL` | Model the agent runs. Defaults to `claude-opus-4-8` (pinned for reproducible conflict resolution); set this variable to override. |

State lives in [`.github/bsc-sync/last-synced-tag`](../.github/bsc-sync/last-synced-tag).
The workflow advances it (in a dedicated commit on the sync branch) once tags
are merged, so the next run picks up where this one left off. Seed value:
`v1.7.3` (the COR-27 sync).

## Manual runs

From the **Actions → BSC Upstream Sync → Run workflow** menu:

- `tags` — an explicit space-separated tag list (e.g. `v1.7.4 v1.7.5`),
  overriding auto-detection. Useful for re-running a single tag, or for
  deliberately syncing a **major release** (the major-version gate is bypassed
  for explicit lists — the human has chosen those tags on purpose).
- `include_prerelease` — include `-alpha`/`-beta`/`-rc`/`-feature` tags during
  auto-detection (default: stable `vX.Y.Z` only).

## Components

| Path | Role |
|------|------|
| `.github/workflows/bsc-upstream-sync.yml` | Orchestration: schedule, env setup, PR + Slack. |
| `.github/scripts/bsc-sync/detect-new-tags.sh` | Lists upstream tags newer than the last-synced one. |
| `.github/scripts/bsc-sync/run-sync.sh` | The merge loop + agent handoff + report generation. |
| `.github/scripts/bsc-sync/resolve-prompt.md` | The agent's conflict-resolution prompt template. |
| `.github/scripts/bsc-sync/impact-prompt.md` | The agent's release-impact report prompt template. |
| `.github/bsc-sync/last-synced-tag` | Sync state (last BSC tag merged). |

Both scripts are runnable locally for testing:

```bash
# What would the next run merge?
.github/scripts/bsc-sync/detect-new-tags.sh

# Dry-run the merge loop without the agent (conflicts reported, not resolved):
git checkout -b bsc-sync/local-test
SKIP_AGENT=1 .github/scripts/bsc-sync/run-sync.sh v1.7.4 v1.7.5
```

## Notes & limitations

- The `genesis` submodule pointer is never touched (checkout uses
  `submodules: false`).
- The merge strategy mirrors the documented manual method (tag by tag,
  Chiliz customisations win). The squash-merge graft trick described in the
  team runbook is only needed for the *first* sync after a squash-merged
  history; once a real merge commit exists, later tags get correct
  merge-bases automatically.
- Deep business-logic conflicts are intentionally **out of scope** for the
  agent — those are escalated to a human via the 🔴 flag.
