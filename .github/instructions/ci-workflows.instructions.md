---
applyTo: ".github/workflows/**,.github/scripts/**"
---

# Reviewing CI workflows and scripts

This is unattended automation that pages people at night and, in the case of the sync workflow,
drives an agent over consensus code. Review it like production.

## Shell and workflow rigor

- `set -euo pipefail` in scripts, and be alert to the cases where `set -e` does **not** save you:
  inside `if`/`&&` conditions, in a pipeline's non-final command without `pipefail`, and in a
  subshell whose status is discarded.
- Validate argument counts before `shift N`. A value-taking option at the end of the argv list
  makes `shift 2` fail; without `set -e` the parse loop then spins forever.
- Suppressed errors that fake success: `2>/dev/null`, `|| true` and `continue-on-error` that turn
  "the tool failed to run" into "the tool found nothing". A package that fails to compile must not
  be reported as having no fuzz targets.
- Notification and cleanup steps: `failure()` is false for a cancelled or timed-out run, so an
  unattended weekly job needs `cancelled()` too. And a notifier that lives in the repo is
  unavailable on exactly the failure path where checkout failed — check that the failure path it
  claims to cover actually works.
- Glob depth differs by tool, so check which one is in play before calling a pathspec wrong. A
  **git** pathspec without `:(glob)` magic uses fnmatch *without* `FNM_PATHNAME`, so `*` spans `/`:
  `git ls-files '*/testdata/fuzz/*'` does match `consensus/parlia/testdata/fuzz/<target>/<input>`,
  six segments deep. A shell glob and `find -path` behave differently. The pathspec in `fuzz.yml`
  is correct and carries a comment saying so — do not "fix" it.
- Secrets never land in logs or in `set -x` output; `GITHUB_TOKEN` permissions stay minimal;
  third-party actions are pinned.

## What belongs in `.github/workflows/`

**Chiliz-owned workflows only.** The current set is: `bsc-upstream-sync.yml`, `build.yml`,
`build-test.yml`, `deploy.yml`, `docker-release.yml`, `evm-tests.yml`, `fuzz.yml`, `lint.yml`,
`nancy.yml`, `pre-release.yml`, `release.yml`, `unit-test.yml`.

Upstream BSC/go-ethereum workflows must be **deleted on every sync**, and they come back two ways:

- a *new* upstream workflow merges in with no conflict, because the merge base never had it
  (`validate_pr.yml` from go-ethereum slipped in during the v1.6.3→v1.7.3 sync);
- a previously-deleted one returns as a modify/delete conflict — it stays deleted.

Known repeat offenders: `validate_pr.yml`, `commit-lint.yml`, `integration-test.yml`. **Flag any
workflow file added by a sync PR that is not in the list above.**

## The sync tooling is ours

`.github/scripts/bsc-sync/` (`run-sync.sh`, `detect-new-tags.sh`, `verify-merge.sh`, and the
`*-prompt.md` agent prompts), `.github/bsc-sync/last-synced-tag`, and `docs/bsc-upstream-sync.md`
are Chiliz-owned and never take an upstream side. `last-synced-tag` is advanced by the workflow,
never by a merge.

When reviewing a change to the agent prompts, remember they are the safety rail on an agent editing
consensus code: check that the invariant list, the confidence taxonomy and the output schema stay
intact, and that the workflow still opens a **draft** PR and never auto-merges.

## Fuzz campaign

`fuzz.yml` and `.github/scripts/fuzz/run-fuzz.sh` discover and run every `Fuzz*` target in the
Chiliz packages weekly, ratcheted against `.github/scripts/fuzz/expected-targets.txt` — a
per-package **minimum-count** ratchet that catches discovery breaking, not a registry of targets.
Counts only go up; a package carrying more targets than recorded is a notice, never a failure.
Deliberately removing a package from `fuzz.yml` means dropping its line there in the same PR, which
is what keeps `--require-complete` a decision rather than an accident.
