You are writing a **Release Impact** report for a sync that merged one or
more upstream **bnb-chain/bsc** release tags into **chiliz-chain** (Chiliz
Chain v2, "CC2"), a customised fork of BSC. You are running headless inside a
GitHub Actions job, with no human to ask. The merge has already happened and
is committed — your job is to explain, for a human reviewer, what this release
does to our code.

## Context you are given

- Pre-sync base commit (our tree before the sync): `{{BASE}}`
- Merged tags, in order: {{TAGS}}
- Current `HEAD` is the fully merged result.

## Required reading

Read `CLAUDE.md` at the repository root first. Its **Invariants** section
lists the Chiliz-specific code and config that must never be lost to upstream
(EVM hooks, Parlia tokenomics/inflation, the Pepper8/Pipe8/Snake8/Dragon8
forks, gas & fee constants, chain config & fork schedules, tracing). Those are
the areas a reviewer cares about most.

## How to investigate (do not modify anything)

- `git log --oneline --no-merges {{BASE}}..HEAD` — every change that landed.
- `git log --oneline {{BASE}}..<tag>` and `git show -s <tag>` — per-tag scope
  and any annotated release notes.
- `git diff {{BASE}} HEAD -- <path>` — the net change to a specific file/area.
  Focus the value-level analysis on the invariant/config files called out in
  `CLAUDE.md`, e.g. `params/protocol_params.go`, `params/config.go`,
  `consensus/parlia/`, `consensus/misc/eip1559/`, `core/vm/evm.go`,
  `config/embedded/*.json`.
- Use only local git data (commit messages, tag annotations, diffs). You have
  no internet access — do not attempt to fetch external release notes.

Cover changes that merged **cleanly** too, not just conflicts — the bulk of a
release lands without conflict and is the easy thing to miss.

## Output — write a Markdown report

Write the report to exactly this path: `{{IMPACT_PATH}}`

Use this structure (omit a section only if it is genuinely empty, and say so):

```markdown
## Release impact

### What this release changes
A short, plain-English digest of the main features, fixes, and protocol
changes in the merged tag(s). Bullet points, grouped by theme. Cite the tag
where it helps (e.g. "(v1.7.4)").

### Config & parameter changes
A table of changed configuration / constants, especially in the invariant
areas. Show what moved and how it was resolved:

| File | Setting | Upstream value | Our value (after sync) | Notes |
|------|---------|----------------|------------------------|-------|

"Our value (after sync)" is what is in the tree now at HEAD. In Notes, say
whether we kept the Chiliz value, adopted upstream's, or merged both — and why
it matters. If the release changed no config/constants, write "None."

### Invariant impact ⚠️
Explicitly call out anything upstream changed that *touches* a Chiliz
invariant (per CLAUDE.md) — even if it merged cleanly. For each: the file/area,
what upstream did, and whether our customisation is still intact. If nothing
touched an invariant, say so clearly — that itself is a useful all-clear.

### Noteworthy clean-merged changes
Significant changes that merged without conflict and that a reviewer should
still eyeball (new hardforks added to config, new replay/trace code paths, fee
or base-fee logic, txpool/consensus internals). Skip routine dependency bumps
and refactors.
```

Keep it tight and high-signal — a reviewer should be able to read it in a
couple of minutes and know where to look. Do not run `git add`, `git commit`,
or any command that changes the working tree or history.
