# GitHub Copilot code review on this repository

Copilot code review runs on pull requests here. This document describes the context we feed it,
how to change that context, and the one-off admin steps that are not in the repository.

## What Copilot reads

Copilot reads all of this from the **head branch** of the PR under review — the branch with the
changes, not the base. That means a PR can change its own review context, which is how we test
these files (see [Testing a change](#testing-a-change)).

| File | Scope | Purpose |
|---|---|---|
| `.github/copilot-instructions.md` | every review | Short repo-wide policy: the consensus-break rule, the "do not flag" list, where the detail lives. Kept to roughly two pages on purpose — always-on context that grows stops being read. |
| `.github/instructions/*.instructions.md` | files matching the `applyTo` glob | Per-area review checks. Repo-wide and path-scoped instructions are **combined**, not overridden. |
| `.github/skills/code-review/` | on demand | The deep material: `invariants.md`, `regressions.md`, `upstream-sync-review.md`. Copilot pulls a skill in when it judges it relevant, which is why the `description` field in `SKILL.md` matters more than the body. |
| `CLAUDE.md` | every review | Picked up automatically (Copilot reads `CLAUDE.md` / `AGENTS.md` / `REVIEW.md`). It is the merge runbook, useful as background. |

We deliberately do **not** have an `AGENTS.md`: it would compete with `CLAUDE.md` for the same slot
for no gain.

### Editing the instruction files

- Path-scoped files need `applyTo:` frontmatter with comma-separated globs. `excludeAgent` limits a
  file to a subset of the consumers; GitHub documents exactly two values today, `"code-review"` and
  `"cloud-agent"` ("Use either `code-review` or `cloud-agent`"). An unrecognised value is not an
  error anywhere, so a typo silently leaves the file applying to everything — check a value against
  the docs before relying on it.
- Write checks, not merge advice: "flag if X" rather than "preserve X during a merge". The same
  fact reads very differently to a reviewer and to a merger.
- After editing a glob, confirm it matches something real:

```bash
git ls-files | grep -E 'consensus/parlia/|core/vm/evm\.go'
```

- These files duplicate parts of `CLAUDE.md` by design — different audience, different framing.
  When an invariant changes, update both.

## MCP servers

MCP gives Copilot read-only access to systems outside the repository during a review. Tool calls
in review are read-only by design.

Configured in **repository Settings → Copilot → MCP servers**; credentials go in **Settings →
Secrets and variables → Agents**. Both need repository admin. The toggle *"Allow Copilot to use MCP
tools when reviewing pull requests"* must stay enabled.

Intended configuration:

| Server | State | Why |
|---|---|---|
| GitHub | enabled (default) | Linked issues, prior PRs, CI results. |
| Linear | **to add**, read-only | Every PR title names a `COR-xxx` issue. With Linear connected, Copilot can read the issue and review the diff against stated intent — scope drift, unmet acceptance criteria. |
| Playwright | **to disable** (on by default) | A browser driver has nothing to do with a Go L1 node; it is a distraction in tool selection. |

Comments generated with skill or MCP context carry an **attribution** at the bottom naming the
source. That is the only way to confirm the context was actually used.

## Testing a change

Because Copilot reads from the head branch, changes are testable before merge.

1. Open the PR normally and request a Copilot review on it. This only proves the run succeeds —
   the diff is markdown, so expect few comments.
2. For a real signal, a throwaway branch carrying the instruction files **and two deliberate
   invariant breaks**:
   - remove the `evm.warmDeployerProxyOnHookDispatch(addr)` line from `core/vm/evm.go` (COR-193);
   - swap `InitialBaseFeeForBSC` for `InitialBaseFee` at a Parlia call site (COR-214).

   Success is Copilot flagging both. The baseline is that it flagged neither class across
   PRs #78–#82.

   > **This branch contains two known consensus breaks and must never reach `develop`.** Copilot
   > review needs a PR, and `build-test.yml`, `evm-tests.yml` and `lint.yml` all pass on it —
   > green CI on a branch that makes historical blocks unimportable. Bake the guards into the
   > steps rather than relying on a note:
   >
   > - open it **as a draft**;
   > - base it on the **instruction-files branch, not `develop`** — that also keeps the diff to
   >   just the two breaks, which is what makes the test readable;
   > - prefix the title **`DO NOT MERGE`**;
   > - say nothing in the title, body, branch name or commit message about *what* was planted —
   >   the review reads all of them and will restate your description instead of finding the bug;
   > - close the PR and delete the branch as soon as the review has been read.

3. Check for attributions on the consensus-file comments. No attribution means the skill was not
   picked up — tighten the `description` in `SKILL.md`, since that is what Copilot matches on.
   A cheaper signal that the skill is registered at all: the footer hint drops its "Add a
   code-review agent skill" half once `SKILL.md` exists, leaving only the MCP suggestion.

## Related

- `CLAUDE.md` — the upstream-merge runbook and the authoritative invariant list.
- `docs/bsc-upstream-sync.md` — the automated sync, whose agent prompts in
  `.github/scripts/bsc-sync/` are the model these review files follow.
